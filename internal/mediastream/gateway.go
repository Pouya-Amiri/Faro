package mediastream

import (
	"bufio"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

const (
	defaultGatewayChunk = int64(256 * 1024)
	defaultGatewayCache = int64(64 * 1024 * 1024)
	maxGatewayCache     = int64(256 * 1024 * 1024)
)

type GatewayConfig struct {
	Viewer     streamtransport.Viewer
	Capability string
	Media      protocol.Media
	ChunkBytes int64
	CacheBytes int64
}

type Gateway struct {
	viewer     streamtransport.Viewer
	capability string
	media      protocol.Media
	chunkBytes int64
	path       string
	listener   net.Listener
	server     *http.Server

	cacheMu    sync.Mutex
	cache      map[int64]*list.Element
	cacheOrder *list.List
	cacheSize  int64
	cacheLimit int64
	inflight   map[int64]*chunkFetch
	fetchSlots chan struct{}
	closed     bool
	closeOnce  sync.Once
}

type cachedChunk struct {
	start int64
	data  []byte
}

type chunkFetch struct {
	done  chan struct{}
	data  []byte
	err   error
	retry bool
}

func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	if cfg.Viewer == nil || len(cfg.Capability) < 32 || cfg.Media.SizeBytes <= 0 || !validFileFingerprint(cfg.Media.Fingerprint) {
		return nil, errors.New("stream gateway configuration is invalid")
	}
	chunkBytes := cfg.ChunkBytes
	if chunkBytes == 0 {
		chunkBytes = defaultGatewayChunk
	}
	if chunkBytes < 1 || chunkBytes > defaultMaxRange {
		return nil, errors.New("stream gateway chunk size must be between 1 byte and 4 MiB")
	}
	cacheBytes := cfg.CacheBytes
	if cacheBytes == 0 {
		cacheBytes = defaultGatewayCache
	}
	if cacheBytes < chunkBytes || cacheBytes > maxGatewayCache {
		return nil, errors.New("stream gateway cache must hold at least one chunk and at most 256 MiB")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start stream loopback gateway: %w", err)
	}
	token, err := randomPath()
	if err != nil {
		listener.Close()
		return nil, err
	}
	gateway := &Gateway{
		viewer: cfg.Viewer, capability: cfg.Capability, media: cfg.Media, chunkBytes: chunkBytes,
		path: "/" + token, listener: listener, cacheLimit: cacheBytes,
		cache: make(map[int64]*list.Element), cacheOrder: list.New(), inflight: make(map[int64]*chunkFetch),
		fetchSlots: make(chan struct{}, defaultConcurrency),
	}
	gateway.server = &http.Server{
		Handler: gateway, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
	}
	go func() { _ = gateway.server.Serve(listener) }()
	return gateway, nil
}

func (g *Gateway) URL() string { return "http://" + g.listener.Addr().String() + g.path }

func (g *Gateway) Close() error {
	var result error
	g.closeOnce.Do(func() {
		g.cacheMu.Lock()
		g.closed = true
		clear(g.cache)
		g.cacheOrder.Init()
		g.cacheSize = 0
		g.cacheMu.Unlock()
		result = errors.Join(g.server.Close(), g.viewer.Close())
	})
	return result
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != g.path {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodHead && request.Method != http.MethodGet {
		writer.Header().Set("Allow", "HEAD, GET")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(g.media.Title))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("ETag", `"`+g.media.Fingerprint+`"`)
	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Length", strconv.FormatInt(g.media.SizeBytes, 10))
		writer.WriteHeader(http.StatusOK)
		return
	}
	start, end, partial, err := parsePlayerRange(request.Header.Get("Range"), g.media.SizeBytes)
	if err != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", g.media.SizeBytes))
		http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, g.media.SizeBytes))
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	for offset := start; offset <= end; {
		chunkStart := offset / g.chunkBytes * g.chunkBytes
		begin := offset - chunkStart
		count := min(end-offset+1, g.chunkBytes-begin)
		if err := g.writeChunk(request.Context(), writer, chunkStart, begin, count); err != nil {
			return
		}
		offset += count
	}
}

func (g *Gateway) writeChunk(ctx context.Context, writer io.Writer, start, begin, count int64) error {
	for {
		g.cacheMu.Lock()
		if g.closed {
			g.cacheMu.Unlock()
			return net.ErrClosed
		}
		if cached := g.cache[start]; cached != nil {
			g.cacheOrder.MoveToFront(cached)
			data := cached.Value.(*cachedChunk).data
			g.cacheMu.Unlock()
			return writeGatewayBytes(writer, data[begin:begin+count])
		}
		if pending := g.inflight[start]; pending != nil {
			g.cacheMu.Unlock()
			select {
			case <-pending.done:
				if (errors.Is(pending.err, context.Canceled) || pending.retry) && ctx.Err() == nil {
					continue
				}
				if pending.err != nil {
					return pending.err
				}
				return writeGatewayBytes(writer, pending.data[begin:begin+count])
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pending := &chunkFetch{done: make(chan struct{})}
		g.inflight[start] = pending
		g.cacheMu.Unlock()

		end := min(g.media.SizeBytes-1, start+g.chunkBytes-1)
		writeFailed := false
		pending.data, pending.err = g.fetchRange(ctx, start, end, func(offset int64, data []byte) error {
			first := max(begin, offset)
			last := min(begin+count, offset+int64(len(data)))
			if first >= last {
				return nil
			}
			if err := writeGatewayBytes(writer, data[first-offset:last-offset]); err != nil {
				writeFailed = true
				return err
			}
			return nil
		})
		pending.retry = writeFailed
		if pending.err != nil && ctx.Err() != nil {
			pending.err = ctx.Err()
		}
		g.cacheMu.Lock()
		delete(g.inflight, start)
		if pending.err == nil && !g.closed {
			entry := g.cacheOrder.PushFront(&cachedChunk{start: start, data: pending.data})
			g.cache[start] = entry
			g.cacheSize += int64(len(pending.data))
			for g.cacheSize > g.cacheLimit {
				oldest := g.cacheOrder.Back()
				old := oldest.Value.(*cachedChunk)
				delete(g.cache, old.start)
				g.cacheOrder.Remove(oldest)
				g.cacheSize -= int64(len(old.data))
			}
		}
		close(pending.done)
		g.cacheMu.Unlock()
		return pending.err
	}
}

func writeGatewayBytes(writer io.Writer, data []byte) error {
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (g *Gateway) fetchRange(ctx context.Context, start, end int64, onData func(int64, []byte) error) ([]byte, error) {
	select {
	case g.fetchSlots <- struct{}{}:
		defer func() { <-g.fetchSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := g.viewer.WaitForDirect(ctx); err != nil {
		return nil, err
	}
	conn, err := g.viewer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://faro.media"+upstreamPath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+g.capability)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	request.Close = true
	if err := request.Write(conn); err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	want := end - start + 1
	if response.StatusCode != http.StatusPartialContent || response.ContentLength != want {
		return nil, fmt.Errorf("stream provider returned %s for range %d-%d", response.Status, start, end)
	}
	data := make([]byte, want)
	for offset := int64(0); offset < want; {
		batch := min(want-offset, 16*1024)
		read, err := io.ReadFull(response.Body, data[offset:offset+batch])
		if read > 0 {
			if writeErr := onData(offset, data[offset:offset+int64(read)]); writeErr != nil {
				return nil, writeErr
			}
			offset += int64(read)
		}
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func parsePlayerRange(value string, size int64) (start, end int64, partial bool, err error) {
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, errors.New("only one byte range is supported")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid byte range")
	}
	if parts[0] == "" {
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix <= 0 {
			return 0, 0, false, errors.New("invalid suffix range")
		}
		suffix = min(suffix, size)
		return size - suffix, size - 1, true, nil
	}
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid range start")
	}
	end = size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid range end")
		}
		end = min(end, size-1)
	}
	return start, end, true, nil
}

func randomPath() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
