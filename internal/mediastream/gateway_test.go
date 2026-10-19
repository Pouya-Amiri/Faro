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
	gateway    *Gateway
	content    []byte
	source     *Source
	network    *streamtransport.FakeNetwork
	publisher  streamtransport.Publisher
	capability string
	mu         sync.Mutex
	ranges     []string
}

func newGatewayFixture(t *testing.T, content []byte, maxRange int64, configure func(*GatewayConfig)) *gatewayFixture {
	t.Helper()
	source := newTestSource(t, content, maxRange)
	capability := strings.Repeat("g", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	network := streamtransport.NewFakeNetwork()
	fixture := &gatewayFixture{content: content, source: source, network: network, capability: capability}
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
	fixture.publisher = publisher
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
	if strings.Contains(fixture.gateway.URL(), fixture.gateway.up.capability) {
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

func TestGatewayURLEndsWithTheFileName(t *testing.T) {
	content := randomContent(t, 4096)
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.Media.Title = "My Movie (2024) #1.mkv"
		cfg.DisableBackgroundFill = true
	})
	address := fixture.gateway.URL()
	if !strings.HasSuffix(address, "/My%20Movie%20%282024%29%20%231.mkv") {
		t.Fatalf("gateway URL %q does not end with the file name", address)
	}
	// Players may escape the name differently; the decoded path is what counts.
	for _, variant := range []string{address, strings.Replace(address, "%282024%29", "(2024)", 1)} {
		response, err := http.Get(variant)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != string(content) {
			t.Fatalf("%s: status %d", variant, response.StatusCode)
		}
	}
	if response, err := http.Get(strings.TrimSuffix(address, "/My%20Movie%20%282024%29%20%231.mkv")); err == nil {
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("the bare token path answered %d", response.StatusCode)
		}
	}
}

// connectedViewer returns a new viewer the fixture's provider accepts.
func (f *gatewayFixture) connectedViewer(t *testing.T) streamtransport.Viewer {
	t.Helper()
	viewer, err := f.network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.AllowClient(viewer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := viewer.SetConnectionBlob(f.publisher.ConnectionBlob()); err != nil {
		t.Fatal(err)
	}
	return viewer
}

// faultyViewer lets a test decide how each connection to the provider and
// each direct-path check behaves.
type faultyViewer struct {
	streamtransport.Viewer
	dials  atomic.Int32
	checks atomic.Int32
	dial   func(attempt int32, ctx context.Context) (net.Conn, error)
	direct func() error
}

func (v *faultyViewer) WaitForDirect(ctx context.Context) error {
	v.checks.Add(1)
	if v.direct != nil {
		if err := v.direct(); err != nil {
			return err
		}
	}
	return v.Viewer.WaitForDirect(ctx)
}

func (v *faultyViewer) Dial(ctx context.Context) (net.Conn, error) {
	attempt := v.dials.Add(1)
	if v.dial != nil {
		return v.dial(attempt, ctx)
	}
	return v.Viewer.Dial(ctx)
}

// tricklingConn answers one range request with valid headers and then sends
// a kilobyte of content every 250 ms, like a connection that degraded
// mid-transfer.
func tricklingConn(t *testing.T, content []byte) net.Conn {
	client, server := net.Pipe()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); server.Close() })
	go func() {
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		var start, end int64
		fmt.Sscanf(request.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		fmt.Fprintf(server, "HTTP/1.1 206 Partial Content\r\nContent-Length: %d\r\n\r\n", end-start+1)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for position := start; position <= end; position += 1024 {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := server.Write(content[position:min(position+1024, end+1)]); err != nil {
					return
				}
			}
		}
	}()
	return client
}

func shortenHealthChecks(t *testing.T) {
	stall, window, route, tick := stallTimeout, slowWindow, routeInterval, healthTick
	stallTimeout, slowWindow, routeInterval, healthTick = 600*time.Millisecond, 800*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { stallTimeout, slowWindow, routeInterval, healthTick = stall, window, route, tick })
}

func TestGatewayBacksOffAfterAFailedFetch(t *testing.T) {
	content := randomContent(t, 256*1024)
	var viewer *faultyViewer
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.DisableBackgroundFill = true
		viewer = &faultyViewer{Viewer: cfg.Viewer}
		viewer.dial = func(attempt int32, ctx context.Context) (net.Conn, error) {
			if attempt <= 3 {
				return nil, errors.New("provider is restarting")
			}
			return viewer.Viewer.Dial(ctx)
		}
		cfg.Viewer = viewer
	})
	began := time.Now()
	if body := fixture.get(t, ""); string(body) != string(content) {
		t.Fatal("stream returned the wrong bytes after transient failures")
	}
	// Three failures wait 250 ms, 500 ms and 1 s before the next attempt.
	if elapsed := time.Since(began); elapsed < 1500*time.Millisecond {
		t.Fatalf("retries were not spaced out: the read finished after %s", elapsed)
	}
	if progress := fixture.gateway.Progress(); progress.Error != "" {
		t.Fatalf("a successful fetch did not clear the failure: %q", progress.Error)
	}
}

func TestGatewayReadAheadRecoversAfterATransientFailure(t *testing.T) {
	content := randomContent(t, 16*1024*1024)
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.CacheBytes = 8 * 1024 * 1024
		viewer := &faultyViewer{Viewer: cfg.Viewer}
		viewer.dial = func(attempt int32, ctx context.Context) (net.Conn, error) {
			if attempt == 1 {
				return nil, errors.New("one transient connection failure")
			}
			return viewer.Viewer.Dial(ctx)
		}
		cfg.Viewer = viewer
	})
	g := fixture.gateway
	g.mu.Lock()
	g.scheduleReadAheadLocked(0)
	g.mu.Unlock()
	// No player request is waiting: only the retry can resume read-ahead.
	waitUntil(t, "read-ahead never retried after a transient failure", func() bool { return g.received.Load() > 0 })
}

func TestGatewayRestartsAStalledTransfer(t *testing.T) {
	shortenHealthChecks(t)
	content := randomContent(t, 512*1024)
	var viewer *faultyViewer
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.DisableBackgroundFill = true
		viewer = &faultyViewer{Viewer: cfg.Viewer}
		viewer.dial = func(attempt int32, ctx context.Context) (net.Conn, error) {
			if attempt == 1 {
				// Accepts the request and never answers.
				client, server := net.Pipe()
				t.Cleanup(func() { server.Close() })
				go io.Copy(io.Discard, server)
				return client, nil
			}
			return viewer.Viewer.Dial(ctx)
		}
		cfg.Viewer = viewer
	})
	if body := fixture.get(t, ""); string(body) != string(content) {
		t.Fatal("stream returned the wrong bytes after a stalled connection")
	}
	if viewer.dials.Load() < 2 {
		t.Fatal("the stalled connection was never replaced")
	}
}

func TestGatewayRestartsATricklingLink(t *testing.T) {
	shortenHealthChecks(t)
	content := randomContent(t, 2*1024*1024)
	var viewer *faultyViewer
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.DisableBackgroundFill = true
		viewer = &faultyViewer{Viewer: cfg.Viewer}
		viewer.dial = func(attempt int32, ctx context.Context) (net.Conn, error) {
			if attempt == 1 {
				return tricklingConn(t, content), nil
			}
			return viewer.Viewer.Dial(ctx)
		}
		cfg.Viewer = viewer
	})
	began := time.Now()
	if body := fixture.get(t, "bytes=0-1048575"); string(body) != string(content[:1024*1024]) {
		t.Fatal("stream returned the wrong bytes after a trickling connection")
	}
	// At 4 KB/s the first megabyte alone would take over four minutes.
	if elapsed := time.Since(began); elapsed > 10*time.Second || viewer.dials.Load() < 2 {
		t.Fatalf("the trickling connection was not replaced (took %s, %d dials)", elapsed, viewer.dials.Load())
	}
}

func TestGatewayStopsFetchingWhenTheDirectPathIsLost(t *testing.T) {
	shortenHealthChecks(t)
	content := randomContent(t, 2*1024*1024)
	var lost atomic.Bool
	var viewer *faultyViewer
	fixture := newGatewayFixture(t, content, defaultMaxRange, func(cfg *GatewayConfig) {
		cfg.DisableBackgroundFill = true
		viewer = &faultyViewer{Viewer: cfg.Viewer}
		viewer.direct = func() error {
			if lost.Load() {
				return errors.New("no direct path")
			}
			return nil
		}
		viewer.dial = func(attempt int32, ctx context.Context) (net.Conn, error) {
			if attempt == 1 {
				return tricklingConn(t, content), nil
			}
			return viewer.Viewer.Dial(ctx)
		}
		cfg.Viewer = viewer
	})
	g := fixture.gateway
	g.mu.Lock()
	g.scheduleReadAheadLocked(0)
	g.mu.Unlock()
	waitUntil(t, "the transfer never started", func() bool { return g.received.Load() > 0 })
	lost.Store(true)
	waitUntil(t, "a lost direct path was not reported", func() bool { return g.Progress().Route == RouteUnavailable })
	g.mu.Lock()
	running := 0
	for f := range g.fetches {
		if !f.requested.IsZero() {
			running++
		}
	}
	g.mu.Unlock()
	if running != 0 {
		t.Fatalf("%d fetches kept running without a direct path", running)
	}
	lost.Store(false)
	if body := fixture.get(t, "bytes=0-65535"); string(body) != string(content[:65536]) {
		t.Fatal("stream returned the wrong bytes after the direct path returned")
	}
	if route := g.Progress().Route; route != RouteDirect {
		t.Fatalf("route is %q after the direct path returned", route)
	}
}

func TestGatewayReconnectResumesTheTransfer(t *testing.T) {
	content := randomContent(t, 1024*1024)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.ChunkBytes = 64 * 1024
		cfg.DisableBackgroundFill = true
	})
	g := fixture.gateway
	if body := fixture.get(t, "bytes=0-131071"); string(body) != string(content[:131072]) {
		t.Fatal("first range returned the wrong bytes")
	}
	g.Disconnect()
	if route := g.Progress().Route; route != RouteDisconnected {
		t.Fatalf("route is %q after disconnecting", route)
	}
	// Cached bytes stay readable without a provider.
	if body := fixture.get(t, "bytes=0-65535"); string(body) != string(content[:65536]) {
		t.Fatal("cached range was not readable while disconnected")
	}
	result := make(chan []byte, 1)
	go func() { result <- fixture.get(t, "") }()
	select {
	case <-result:
		t.Fatal("an uncached range was served without a provider")
	case <-time.After(300 * time.Millisecond):
	}
	if err := g.Reconnect(fixture.connectedViewer(t), fixture.capability); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-result:
		if string(body) != string(content) {
			t.Fatal("stream returned the wrong bytes after reconnecting")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting player request did not resume after reconnecting")
	}
}

func TestGatewayReportsAProviderThatRefusesTheTransfer(t *testing.T) {
	content := randomContent(t, 512*1024)
	refused := make(chan error, 2)
	fixture := newGatewayFixture(t, content, 64*1024, func(cfg *GatewayConfig) {
		cfg.ChunkBytes = 64 * 1024
		cfg.DisableBackgroundFill = true
		cfg.OnUpstreamRefused = func(err error) { refused <- err }
	})
	fixture.source.Revoke(fixture.capability)
	request, err := http.NewRequest(http.MethodGet, fixture.gateway.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		if response, err := http.DefaultClient.Do(request.WithContext(ctx)); err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case err := <-refused:
		var refusal *UpstreamRefusedError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusUnauthorized {
			t.Fatalf("unexpected refusal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked capability was not reported")
	}
	if route := fixture.gateway.Progress().Route; route != RouteDisconnected {
		t.Fatalf("route is %q after the provider refused", route)
	}
	select {
	case err := <-refused:
		t.Fatalf("refusal reported twice: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestGatewayRejectsImpossibleFileSizes(t *testing.T) {
	network := streamtransport.NewFakeNetwork()
	viewer, err := network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 0, protocol.MaxStreamBytes + 1, 1<<63 - 1} {
		_, err := NewGateway(GatewayConfig{
			Viewer: viewer, Capability: strings.Repeat("c", 32), CacheBytes: 4 * 1024 * 1024,
			Media: protocol.Media{Title: "Movie.mkv", SizeBytes: size, Fingerprint: "file-v1:" + strings.Repeat("a", 64)},
		})
		if err == nil {
			t.Fatalf("a %d-byte file was accepted", size)
		}
	}
}

func TestMemoryCacheEvictionKeepsNoDiskSlots(t *testing.T) {
	store := newMemoryStore(100*1024, 2*1024, 1024)
	data := make([]byte, 1024)
	for block := int64(0); block < 100; block++ {
		if _, err := store.WriteAt(data, block*1024); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.freeSlots) != 0 {
		t.Fatalf("a memory cache recorded %d disk slots", len(store.freeSlots))
	}
}
