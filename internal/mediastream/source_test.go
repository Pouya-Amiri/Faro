package mediastream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

type observedViewer struct {
	streamtransport.Viewer
	dials        atomic.Int32
	directChecks atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (v *observedViewer) WaitForDirect(ctx context.Context) error {
	v.directChecks.Add(1)
	return v.Viewer.WaitForDirect(ctx)
}

func (v *observedViewer) Dial(ctx context.Context) (net.Conn, error) {
	if v.dials.Add(1) == 1 && v.firstStarted != nil {
		close(v.firstStarted)
		select {
		case <-v.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return v.Viewer.Dial(ctx)
}

func newTestSource(t *testing.T, content []byte, maximum int64) *Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(SourceConfig{
		Path: path, MaxRangeBytes: maximum,
		Media: protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	return source
}

func sourceRequest(t *testing.T, source *Source, method, capability, byteRange string) *http.Response {
	t.Helper()
	client, server := net.Pipe()
	go source.HandleConn(server)
	request, err := http.NewRequest(method, "http://faro.media"+upstreamPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	if capability != "" {
		request.Header.Set("Authorization", "Bearer "+capability)
	}
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	if err := request.Write(client); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		response.Body.Close()
		client.Close()
	})
	return response
}

func TestSourceRequiresCapabilityAndBoundedSingleRange(t *testing.T) {
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	source := newTestSource(t, content, 8)
	capability := strings.Repeat("c", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if response := sourceRequest(t, source, http.MethodGet, "", "bytes=0-3"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	response := sourceRequest(t, source, http.MethodGet, capability, "bytes=5-12")
	if response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") != "bytes 5-12/36" {
		t.Fatalf("unexpected range response: %s %#v", response.Status, response.Header)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(content[5:13]) {
		t.Fatalf("unexpected range body: %q", body)
	}
	for _, value := range []string{"", "bytes=0-8", "bytes=0-1,4-5", "bytes=-4"} {
		response := sourceRequest(t, source, http.MethodGet, capability, value)
		if response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("range %q status = %d", value, response.StatusCode)
		}
	}
	source.Revoke(capability)
	if response := sourceRequest(t, source, http.MethodGet, capability, "bytes=0-3"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d", response.StatusCode)
	}
}

func TestSourceRechecksAuthorizationOnReusedConnection(t *testing.T) {
	source := newTestSource(t, []byte("abcdefgh"), 8)
	capability := strings.Repeat("k", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	go source.HandleConn(server)
	reader := bufio.NewReader(client)
	for index, byteRange := range []string{"bytes=0-3", "bytes=4-7"} {
		request, err := http.NewRequest(http.MethodGet, "http://faro.media"+upstreamPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+capability)
		request.Header.Set("Range", byteRange)
		if index == 1 {
			source.Revoke(capability)
			request.Close = true
		}
		if err := request.Write(client); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (response.StatusCode != http.StatusPartialContent || response.Close || string(body) != "abcd") {
			t.Fatalf("first request did not keep the authenticated connection: status=%d close=%t body=%q", response.StatusCode, response.Close, body)
		}
		if index == 1 && response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("revoked capability remained valid on reused connection: status=%d", response.StatusCode)
		}
	}
}

func TestSourceRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "movie.mkv")
	if err := os.WriteFile(target, []byte("movie"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := NewSource(SourceConfig{Path: link, Media: protocol.Media{SizeBytes: 5, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}})
	if err == nil {
		t.Fatal("symlink was accepted as a stream source")
	}
}

func TestGatewayNewRangeDoesNotCancelActiveRead(t *testing.T) {
	content := []byte(strings.Repeat("abcdefgh", 8192))
	source := newTestSource(t, content, 32*1024)
	capability := strings.Repeat("q", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	network := streamtransport.NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), streamtransport.PublisherConfig{HandleConn: source.HandleConn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	viewer, err := network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.AllowClient(viewer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetConnectionBlob(publisher.ConnectionBlob()); err != nil {
		t.Fatal(err)
	}
	observed := &observedViewer{Viewer: viewer, firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	gateway, err := NewGateway(GatewayConfig{Viewer: observed, Capability: capability, Media: media, ChunkBytes: 32 * 1024, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	firstDone := make(chan error, 1)
	go func() {
		request, requestErr := http.NewRequest(http.MethodGet, gateway.URL(), nil)
		if requestErr != nil {
			firstDone <- requestErr
			return
		}
		request.Header.Set("Range", "bytes=0-32767")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			firstDone <- requestErr
			return
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr == nil && string(body) != string(content[:32768]) {
			readErr = errors.New("first range was truncated")
		}
		firstDone <- readErr
	}()
	select {
	case <-observed.firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first upstream read did not start")
	}
	request, err := http.NewRequest(http.MethodGet, gateway.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=32768-65535")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(second) != string(content[32768:]) {
		t.Fatalf("second range failed during active read: bytes=%d error=%v", len(second), err)
	}
	close(observed.releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("new range cancelled an active read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first range did not finish")
	}
}

func TestGatewayWorksWithOneRequestProvider(t *testing.T) {
	content := []byte(strings.Repeat("abcdefgh", 8192))
	source := newTestSource(t, content, 32*1024)
	capability := strings.Repeat("l", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	network := streamtransport.NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), streamtransport.PublisherConfig{HandleConn: func(conn net.Conn) {
		defer conn.Close()
		request, readErr := http.ReadRequest(bufio.NewReader(conn))
		if readErr != nil {
			return
		}
		request.Close = true
		response := source.response(request)
		response.Close = true
		_ = response.Write(conn)
		if response.Body != nil {
			response.Body.Close()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	viewer, err := network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.AllowClient(viewer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetConnectionBlob(publisher.ConnectionBlob()); err != nil {
		t.Fatal(err)
	}
	observed := &observedViewer{Viewer: viewer}
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	gateway, err := NewGateway(GatewayConfig{Viewer: observed, Capability: capability, Media: media, ChunkBytes: 32 * 1024, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	response, err := http.Get(gateway.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != string(content) {
		t.Fatalf("one-request provider returned %d bytes: %v", len(body), err)
	}
}

func TestGatewayStreamsUncachedRangeBeforeFetchCompletes(t *testing.T) {
	content := []byte(strings.Repeat("abcd", 8192))
	firstHalfSent := make(chan struct{})
	releaseRemainder := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseRemainder:
		default:
			close(releaseRemainder)
		}
	})
	network := streamtransport.NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), streamtransport.PublisherConfig{HandleConn: func(conn net.Conn) {
		defer conn.Close()
		request, readErr := http.ReadRequest(bufio.NewReader(conn))
		if readErr != nil {
			return
		}
		request.Body.Close()
		if request.Header.Get("Range") != "bytes=0-32767" {
			return
		}
		fmt.Fprintf(conn, "HTTP/1.1 206 Partial Content\r\nContent-Length: %d\r\n\r\n", len(content))
		if _, writeErr := conn.Write(content[:16384]); writeErr != nil {
			return
		}
		close(firstHalfSent)
		<-releaseRemainder
		_, _ = conn.Write(content[16384:])
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	viewer, err := network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.AllowClient(viewer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetConnectionBlob(publisher.ConnectionBlob()); err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	gateway, err := NewGateway(GatewayConfig{Viewer: viewer, Capability: strings.Repeat("s", 32), Media: media, ChunkBytes: int64(len(content)), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	request, err := http.NewRequest(http.MethodGet, gateway.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-32767")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-firstHalfSent:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not send its first half")
	}
	firstHalf := make(chan error, 1)
	go func() {
		got := make([]byte, 16384)
		_, readErr := io.ReadFull(response.Body, got)
		if readErr == nil && string(got) != string(content[:16384]) {
			readErr = errors.New("gateway changed the first half of the response")
		}
		firstHalf <- readErr
	}()
	select {
	case err := <-firstHalf:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway waited for the entire uncached chunk before sending data")
	}
	close(releaseRemainder)
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != string(content[16384:]) {
		t.Fatalf("gateway remainder failed: bytes=%d error=%v", len(rest), err)
	}
}
