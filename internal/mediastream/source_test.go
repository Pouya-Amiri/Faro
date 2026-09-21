package mediastream

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

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

func TestLoopbackGatewayStreamsAcrossBoundedUpstreamChunks(t *testing.T) {
	content := []byte(strings.Repeat("abcdefghij", 20000))
	source := newTestSource(t, content, 32*1024)
	capability := strings.Repeat("z", 32)
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
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	gateway, err := NewGateway(GatewayConfig{Viewer: viewer, Capability: capability, Media: media, ChunkBytes: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })

	request, err := http.NewRequest(http.MethodGet, gateway.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=12345-156789")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || string(body) != string(content[12345:156790]) {
		t.Fatalf("gateway returned status %d and %d bytes", response.StatusCode, len(body))
	}
	if strings.Contains(gateway.URL(), capability) {
		t.Fatal("gateway URL exposed the transfer capability")
	}
}
