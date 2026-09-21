package mediastream

import (
	"bufio"
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

type GatewayConfig struct {
	Viewer     streamtransport.Viewer
	Capability string
	Media      protocol.Media
	ChunkBytes int64
}

type Gateway struct {
	viewer     streamtransport.Viewer
	capability string
	media      protocol.Media
	chunkBytes int64
	path       string
	listener   net.Listener
	server     *http.Server

	mu            sync.Mutex
	currentCancel context.CancelFunc
	currentID     uint64
	nextID        uint64
	closeOnce     sync.Once
}

func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	if cfg.Viewer == nil || len(cfg.Capability) < 32 || cfg.Media.SizeBytes <= 0 || !validFileFingerprint(cfg.Media.Fingerprint) {
		return nil, errors.New("stream gateway configuration is invalid")
	}
	chunkBytes := cfg.ChunkBytes
	if chunkBytes == 0 {
		chunkBytes = defaultMaxRange
	}
	if chunkBytes < 1 || chunkBytes > defaultMaxRange {
		return nil, errors.New("stream gateway chunk size must be between 1 byte and 4 MiB")
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
		path: "/" + token, listener: listener,
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
		g.mu.Lock()
		if g.currentCancel != nil {
			g.currentCancel()
		}
		g.mu.Unlock()
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
	ctx, cancel := context.WithCancel(request.Context())
	g.mu.Lock()
	if g.currentCancel != nil {
		g.currentCancel()
	}
	g.nextID++
	requestID := g.nextID
	g.currentCancel = cancel
	g.currentID = requestID
	g.mu.Unlock()
	defer func() {
		cancel()
		g.mu.Lock()
		if g.currentID == requestID {
			g.currentCancel = nil
			g.currentID = 0
		}
		g.mu.Unlock()
	}()
	for offset := start; offset <= end; {
		chunkEnd := min(end, offset+g.chunkBytes-1)
		if err := g.fetchRange(ctx, writer, offset, chunkEnd); err != nil {
			return
		}
		offset = chunkEnd + 1
	}
}

func (g *Gateway) fetchRange(ctx context.Context, destination io.Writer, start, end int64) error {
	if err := g.viewer.WaitForDirect(ctx); err != nil {
		return err
	}
	conn, err := g.viewer.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://faro.media"+upstreamPath, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.capability)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	request.Close = true
	if err := request.Write(conn); err != nil {
		return err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	want := end - start + 1
	if response.StatusCode != http.StatusPartialContent || response.ContentLength != want {
		return fmt.Errorf("stream provider returned %s for range %d-%d", response.Status, start, end)
	}
	written, err := io.CopyN(destination, response.Body, want)
	if err != nil {
		return err
	}
	if written != want {
		return io.ErrUnexpectedEOF
	}
	return nil
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
