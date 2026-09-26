package mediastream

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

// gatewayFixture streams content through the fake transport and records the
// ranges the provider was asked for.
type gatewayFixture struct {
	gateway *Gateway
	content []byte
	mu      sync.Mutex
	ranges  []string
}

func newGatewayFixture(t *testing.T, content []byte, maxRange int64, configure func(*GatewayConfig)) *gatewayFixture {
	t.Helper()
	source := newTestSource(t, content, maxRange)
	capability := strings.Repeat("g", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fixture := &gatewayFixture{content: content}
	network := streamtransport.NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), streamtransport.PublisherConfig{HandleConn: func(conn net.Conn) {
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			request, readErr := http.ReadRequest(reader)
			if readErr != nil {
				return
			}
			fixture.mu.Lock()
			fixture.ranges = append(fixture.ranges, request.Header.Get("Range"))
			fixture.mu.Unlock()
			response := source.response(request)
			writeErr := response.Write(conn)
			if response.Body != nil {
				response.Body.Close()
			}
			if writeErr != nil {
				return
			}
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
	cfg := GatewayConfig{
		Viewer: viewer, Capability: capability, CacheDir: t.TempDir(),
		Media: protocol.Media{Title: "Movie.mkv", SizeBytes: int64(len(content)), Fingerprint: "file-v1:" + strings.Repeat("a", 64)},
	}
	if configure != nil {
		configure(&cfg)
	}
	gateway, err := NewGateway(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	fixture.gateway = gateway
	return fixture
}

func (f *gatewayFixture) get(t *testing.T, byteRange string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.gateway.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func randomContent(t *testing.T, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	return content
}

func waitUntil(t *testing.T, message string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGatewayServesRangesAcrossChunksAndCachesThem(t *testing.T) {
	content := randomContent(t, 200_000)
	fixture := newGatewayFixture(t, content, 32*1024, func(cfg *GatewayConfig) {
		cfg.ChunkBytes = 32 * 1024
		cfg.DisableBackgroundFill = true
	})
	if body := fixture.get(t, "bytes=12345-156789"); string(body) != string(content[12345:156790]) {
		t.Fatalf("gateway returned %d wrong bytes", len(body))
	}
	if strings.Contains(fixture.gateway.URL(), fixture.gateway.capability) {
		t.Fatal("gateway URL exposed the transfer capability")
	}
	fetched := fixture.gateway.fetchCount.Load()
	if body := fixture.get(t, "bytes=20000-25000"); string(body) != string(content[20000:25001]) {
		t.Fatal("cached range returned the wrong bytes")
	}
	if got := fixture.gateway.fetchCount.Load(); got != fetched {
		t.Fatalf("a cached range made %d more provider requests", got-fetched)
	}
}

func TestGatewaySeekStartsAtTheRequestedUnit(t *testing.T) {
	content := randomContent(t, 5*1024*1024+12345)
	const seek = 3*1024*1024 + 777
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) { cfg.DisableBackgroundFill = true })
	if body := fixture.get(t, fmt.Sprintf("bytes=%d-%d", seek, seek+1023)); string(body) != string(content[seek:seek+1024]) {
		t.Fatal("seek returned the wrong bytes")
	}
	// Read-ahead requests may reach the provider first; what matters is that
	// nothing before the seek's unit is downloaded.
	fixture.mu.Lock()
	ranges := append([]string(nil), fixture.ranges...)
	fixture.mu.Unlock()
	lowest := int64(-1)
	for _, requested := range ranges {
		var start, end int64
		if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &start, &end); err != nil {
			t.Fatalf("unexpected range %q", requested)
		}
		if lowest < 0 || start < lowest {
			lowest = start
		}
	}
	if lowest > seek || seek-lowest >= unitBytes {
		t.Fatalf("seek to %d fetched from %d, want within one %d-byte unit", seek, lowest, unitBytes)
	}
}

func TestGatewayReadsAhead(t *testing.T) {
	content := randomContent(t, 24*1024*1024)
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) { cfg.DisableBackgroundFill = true })
	if body := fixture.get(t, "bytes=0-1023"); string(body) != string(content[:1024]) {
		t.Fatal("first bytes were wrong")
	}
	// The player asked for 1 KiB; the gateway keeps fetching the window ahead.
	waitUntil(t, "the gateway did not read ahead of the player", func() bool {
		return fixture.gateway.Progress().CachedBytes >= 16*1024*1024
	})
	// A narrow or unmeasured link gets one sequential stream; a fast one
	// fetches in parallel.
	fixture.gateway.mu.Lock()
	narrow := fixture.gateway.foregroundLimitLocked()
	fixture.gateway.rate = fastLinkBytes
	fast := fixture.gateway.foregroundLimitLocked()
	fixture.gateway.rate = 0
	fixture.gateway.mu.Unlock()
	if narrow != 1 || fast != foregroundFetches {
		t.Fatalf("read-ahead limits are %d (narrow) and %d (fast), want 1 and %d", narrow, fast, foregroundFetches)
	}
	fixture.mu.Lock()
	before := len(fixture.ranges)
	fixture.mu.Unlock()
	if body := fixture.get(t, "bytes=8000000-12000000"); string(body) != string(content[8000000:12000001]) {
		t.Fatal("read-ahead range returned the wrong bytes")
	}
	// Reading further moves the read-ahead window on, but nothing already
	// prefetched is requested again.
	fixture.mu.Lock()
	later := append([]string(nil), fixture.ranges[before:]...)
	fixture.mu.Unlock()
	for _, requested := range later {
		var start, end int64
		if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &start, &end); err != nil || start <= 12000000 {
			t.Fatalf("prefetched bytes were requested again: %q", requested)
		}
	}
}

func TestGatewayDownloadsTheWholeFileInTheBackground(t *testing.T) {
	content := randomContent(t, 3*1024*1024+4321)
	fixture := newGatewayFixture(t, content, 256*1024, func(cfg *GatewayConfig) { cfg.ChunkBytes = 256 * 1024 })
	if body := fixture.get(t, "bytes=0-99"); string(body) != string(content[:100]) {
		t.Fatal("first bytes were wrong")
	}
	waitUntil(t, "the background download did not cache the whole file", func() bool {
		progress := fixture.gateway.Progress()
		return progress.CachedBytes == progress.TotalBytes
	})
	fetched := fixture.gateway.fetchCount.Load()
	if body := fixture.get(t, ""); string(body) != string(content) {
		t.Fatal("the fully cached file differs from the original")
	}
	if got := fixture.gateway.fetchCount.Load(); got != fetched {
		t.Fatalf("a fully cached file made %d more provider requests", got-fetched)
	}
}

func TestGatewayMemoryCacheStaysBounded(t *testing.T) {
	content := randomContent(t, 200_000)
	fixture := newGatewayFixture(t, content, 32*1024, func(cfg *GatewayConfig) {
		cfg.ChunkBytes = 32 * 1024
		cfg.CacheBytes = 64 * 1024
	})
	for _, start := range []int{0, 65536, 131072, 0} {
		if body := fixture.get(t, fmt.Sprintf("bytes=%d-%d", start, start+32767)); string(body) != string(content[start:start+32768]) {
			t.Fatalf("range at %d returned the wrong bytes", start)
		}
		store := fixture.gateway.store.(*blockStore)
		store.mu.Lock()
		blocks := len(store.blocks)
		store.mu.Unlock()
		if blocks > 2 {
			t.Fatalf("memory cache holds %d blocks, limit is 2", blocks)
		}
	}
}

func TestGatewayDiskCacheLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	content := randomContent(t, 300_000)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.CacheDir = dir
		cfg.ChunkBytes = 64 * 1024
	})
	if _, ok := fixture.gateway.store.(*diskStore); !ok {
		t.Fatalf("expected a disk cache, got %T", fixture.gateway.store)
	}
	if body := fixture.get(t, ""); string(body) != string(content) {
		t.Fatal("disk-cached stream returned the wrong bytes")
	}
	leftovers := func() []string { matches, _ := filepath.Glob(filepath.Join(dir, "stream-*.part")); return matches }
	if runtime.GOOS != "windows" && len(leftovers()) != 0 {
		t.Fatalf("an open cache file is still named on disk: %v", leftovers())
	}
	if err := fixture.gateway.Close(); err != nil {
		t.Fatal(err)
	}
	if files := leftovers(); len(files) != 0 {
		t.Fatalf("cache files left after close: %v", files)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("cache directory not empty after close: %d entries", len(entries))
	}
}

func TestGatewayDiskCacheLimitKeepsRecentBlocks(t *testing.T) {
	previous := minDiskCacheLimit
	minDiskCacheLimit = 0
	t.Cleanup(func() { minDiskCacheLimit = previous })
	content := randomContent(t, 1024*1024)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.CacheDir = t.TempDir()
		cfg.ChunkBytes = 64 * 1024
		cfg.DiskCacheLimit = 256 * 1024
	})
	store, ok := fixture.gateway.store.(*blockStore)
	if !ok || !store.onDisk() {
		t.Fatalf("expected a bounded disk cache, got %T", fixture.gateway.store)
	}
	// Read the whole file twice: the second pass needs evicted blocks again.
	for pass := range 2 {
		if body := fixture.get(t, ""); string(body) != string(content) {
			t.Fatalf("pass %d returned the wrong bytes", pass)
		}
	}
	store.mu.Lock()
	blocks, slots := len(store.blocks), store.nextSlot
	store.mu.Unlock()
	if blocks > 4 || slots > 4 {
		t.Fatalf("disk cache holds %d blocks in %d slots, limit is 4", blocks, slots)
	}
	if progress := fixture.gateway.Progress(); progress.CachedBytes > 256*1024 {
		t.Fatalf("%d bytes marked cached, limit is %d", progress.CachedBytes, 256*1024)
	}
}

type failingStore struct {
	cacheStore
	failAfter int64
	written   atomic.Int64
}

func (s *failingStore) WriteAt(data []byte, offset int64) ([][2]int64, error) {
	if s.written.Add(int64(len(data))) > s.failAfter {
		return nil, errors.New("no space left on device")
	}
	return s.cacheStore.WriteAt(data, offset)
}

func TestGatewayFallsBackToMemoryWhenTheDiskFills(t *testing.T) {
	content := randomContent(t, 1024*1024)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.CacheDir = t.TempDir()
		cfg.ChunkBytes = 64 * 1024
		cfg.DisableBackgroundFill = true
	})
	fixture.gateway.mu.Lock()
	fixture.gateway.store = &failingStore{cacheStore: fixture.gateway.store, failAfter: 200 * 1024}
	fixture.gateway.mu.Unlock()
	if body := fixture.get(t, ""); string(body) != string(content) {
		t.Fatal("stream returned the wrong bytes after the disk cache failed")
	}
	fixture.gateway.mu.Lock()
	store, ok := fixture.gateway.store.(*blockStore)
	fixture.gateway.mu.Unlock()
	if !ok || store.onDisk() {
		t.Fatalf("expected the memory cache after a disk failure, got %T", fixture.gateway.store)
	}
}

func TestGatewayReportsCachedRanges(t *testing.T) {
	content := randomContent(t, 1024*1024)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.CacheDir = t.TempDir()
		cfg.ChunkBytes = 64 * 1024
		cfg.DisableBackgroundFill = true
	})
	if ranges := fixture.gateway.CachedRanges(8); len(ranges) != 0 {
		t.Fatalf("new gateway reports cached ranges %v", ranges)
	}
	fixture.gateway.mu.Lock()
	for _, unit := range []int64{0, 1, 4, 6, 15} {
		fixture.gateway.have[0] |= 1 << unit
	}
	fixture.gateway.mu.Unlock()
	if got := fixture.gateway.CachedRanges(8); fmt.Sprint(got) != "[[0 0.125] [0.25 0.3125] [0.375 0.4375] [0.9375 1]]" {
		t.Fatalf("cached ranges = %v", got)
	}
	// Two ranges: the smallest gaps (units 2-3 and 5) merge first.
	if got := fixture.gateway.CachedRanges(2); fmt.Sprint(got) != "[[0 0.4375] [0.9375 1]]" {
		t.Fatalf("merged cached ranges = %v", got)
	}
}
