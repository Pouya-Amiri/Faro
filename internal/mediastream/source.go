// Package mediastream serves explicitly selected regular files over Faro's
// authenticated data plane and exposes them to players through loopback HTTP.
package mediastream

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	upstreamPath       = "/v1/media"
	defaultMaxRange    = int64(4 * 1024 * 1024)
	defaultConcurrency = 8
	maxRequestBytes    = int64(64 * 1024)
)

type SourceConfig struct {
	Path               string
	Media              protocol.Media
	MaxRangeBytes      int64
	MaxConcurrentReads int
}

type Source struct {
	file        *os.File
	media       protocol.Media
	contentType string
	maxRange    int64
	reads       chan struct{}

	mu           sync.Mutex
	capabilities map[[sha256.Size]byte]time.Time
	closed       bool
}

func NewSource(cfg SourceConfig) (*Source, error) {
	info, err := os.Lstat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect stream source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("stream source must be a regular file, not a symlink or directory")
	}
	file, err := os.Open(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("open stream source: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect opened stream source: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		file.Close()
		return nil, errors.New("stream source changed while it was being opened")
	}
	if cfg.Media.SizeBytes != openedInfo.Size() || cfg.Media.SizeBytes <= 0 || !validFileFingerprint(cfg.Media.Fingerprint) {
		file.Close()
		return nil, errors.New("stream source does not match its file-v1 media metadata")
	}
	maxRange := cfg.MaxRangeBytes
	if maxRange == 0 {
		maxRange = defaultMaxRange
	}
	if maxRange < 1 || maxRange > 64*1024*1024 {
		file.Close()
		return nil, errors.New("stream source maximum range must be between 1 byte and 64 MiB")
	}
	concurrency := cfg.MaxConcurrentReads
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	if concurrency < 1 || concurrency > 128 {
		file.Close()
		return nil, errors.New("stream source concurrency must be between 1 and 128")
	}
	contentType := mime.TypeByExtension(filepath.Ext(cfg.Media.Title))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &Source{
		file: file, media: cfg.Media, contentType: contentType, maxRange: maxRange,
		reads: make(chan struct{}, concurrency), capabilities: make(map[[sha256.Size]byte]time.Time),
	}, nil
}

func validFileFingerprint(value string) bool {
	const prefix = "file-v1:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func (s *Source) Authorize(capability string, expiresAt time.Time) error {
	if len(capability) < 32 || len(capability) > 512 || !expiresAt.After(time.Now()) {
		return errors.New("stream capability or expiry is invalid")
	}
	digest := sha256.Sum256([]byte(capability))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	s.capabilities[digest] = expiresAt
	return nil
}

func (s *Source) Revoke(capability string) {
	digest := sha256.Sum256([]byte(capability))
	s.mu.Lock()
	delete(s.capabilities, digest)
	s.mu.Unlock()
}

func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	clear(s.capabilities)
	s.mu.Unlock()
	return s.file.Close()
}

// HandleConn serves HTTP/1.1 on one authenticated transport connection. The
// caller owns accepting connections; no filesystem path is read from requests.
func (s *Source) HandleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, maxRequestBytes))
	writer := bufio.NewWriter(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	request.Close = true
	response := s.response(request)
	response.Close = true
	request.Body.Close()
	if err := response.Write(writer); err != nil {
		if response.Body != nil {
			response.Body.Close()
		}
		return
	}
	if response.Body != nil {
		response.Body.Close()
	}
	_ = writer.Flush()
}

func (s *Source) response(request *http.Request) *http.Response {
	if request.URL.Path != upstreamPath {
		return errorResponse(request, http.StatusNotFound, "not found")
	}
	if request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
		return errorResponse(request, http.StatusBadRequest, "request body is not allowed")
	}
	if request.Method != http.MethodHead && request.Method != http.MethodGet {
		response := errorResponse(request, http.StatusMethodNotAllowed, "method not allowed")
		response.Header.Set("Allow", "HEAD, GET")
		return response
	}
	if !s.authorized(request.Header.Get("Authorization"), time.Now()) {
		return errorResponse(request, http.StatusUnauthorized, "unauthorized")
	}
	headers := make(http.Header)
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Content-Type", s.contentType)
	headers.Set("ETag", `"`+s.media.Fingerprint+`"`)
	if request.Method == http.MethodHead {
		return newResponse(request, http.StatusOK, headers, nil, s.media.SizeBytes)
	}
	start, end, err := parseBoundedRange(request.Header.Get("Range"), s.media.SizeBytes, s.maxRange)
	if err != nil {
		headers.Set("Content-Range", fmt.Sprintf("bytes */%d", s.media.SizeBytes))
		return newResponse(request, http.StatusRequestedRangeNotSatisfiable, headers, strings.NewReader("invalid range\n"), int64(len("invalid range\n")))
	}
	select {
	case s.reads <- struct{}{}:
	default:
		return errorResponse(request, http.StatusTooManyRequests, "too many concurrent reads")
	}
	length := end - start + 1
	headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, s.media.SizeBytes))
	body := &releaseReadCloser{Reader: io.NewSectionReader(s.file, start, length), release: func() { <-s.reads }}
	return newResponse(request, http.StatusPartialContent, headers, body, length)
}

func (s *Source) authorized(header string, now time.Time) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	digest := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	expiresAt, ok := s.capabilities[digest]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(s.capabilities, digest)
		return false
	}
	return true
}

func parseBoundedRange(value string, size, maximum int64) (int64, int64, error) {
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, errors.New("a single byte range is required")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, errors.New("range start is required")
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, errors.New("range start is invalid")
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start || end >= size {
			return 0, 0, errors.New("range end is invalid")
		}
	}
	if end-start+1 > maximum {
		return 0, 0, errors.New("range exceeds configured maximum")
	}
	return start, end, nil
}

func errorResponse(request *http.Request, status int, message string) *http.Response {
	body := []byte(message + "\n")
	return newResponse(request, status, http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}, bytes.NewReader(body), int64(len(body)))
}

func newResponse(request *http.Request, status int, headers http.Header, body io.Reader, length int64) *http.Response {
	var closer io.ReadCloser
	if body != nil && request.Method != http.MethodHead {
		if value, ok := body.(io.ReadCloser); ok {
			closer = value
		} else {
			closer = io.NopCloser(body)
		}
	}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: headers, Body: closer, ContentLength: length, Close: request.Close,
		Request: request,
	}
}

type releaseReadCloser struct {
	io.Reader
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Close() error {
	r.once.Do(r.release)
	return nil
}
