package mediastream

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

const (
	defaultGatewayChunk = defaultMaxRange
	// unitBytes is the granularity at which the gateway tracks cached bytes.
	// Fetches start on a unit boundary, so a seek re-downloads at most this
	// much before the requested byte.
	unitBytes = int64(64 * 1024)
	// readAheadBytes is fetched ahead of the player's read position, split
	// into chunks downloaded in parallel.
	readAheadBytes = int64(64 * 1024 * 1024)
	// foregroundFetches serve the player and the read-ahead window;
	// backgroundFetches download the rest of the file while bandwidth allows.
	foregroundFetches = 4
	backgroundFetches = 2
	// memoryCacheBytes bounds the fallback cache when no disk cache is possible.
	memoryCacheBytes = int64(512 * 1024 * 1024)
	maxGatewayCache  = int64(4 * 1024 * 1024 * 1024)
	fetchRetryDelay  = 250 * time.Millisecond
	// minAdaptiveChunk is where fetch sizing starts. On a narrow link small
	// pieces arrive quickly and in playback order; on a fast link fetches
	// grow towards the chunk size so parallel requests hide the latency.
	minAdaptiveChunk = int64(1024 * 1024)
	// chunkDuration is how long one fetch should take at the measured rate.
	chunkDuration = 750 * time.Millisecond
	// fastLinkBytes separates links where parallel fetches help (latency
	// bound) from narrow ones (bandwidth bound, typical home uploads), where
	// they only split the bandwidth the player's next bytes need.
	fastLinkBytes = 50e6 / 8
	// pipelineBytes: the next sequential fetch is requested this close to the
	// end of the current one, hiding the request round trip.
	pipelineBytes = int64(1024 * 1024)
	servePiece    = 256 * 1024
)

type GatewayConfig struct {
	Viewer     streamtransport.Viewer
	Capability string
	Media      protocol.Media
	ChunkBytes int64
	// CacheBytes forces a memory cache of this size instead of a disk cache.
	CacheBytes int64
	// CacheDir holds the disk cache; empty uses the user cache directory.
	CacheDir string
	// DisableBackgroundFill keeps the gateway to what the player asks for
	// plus the read-ahead window.
	DisableBackgroundFill bool
}

// Gateway exposes a streamed file to the local player over loopback HTTP. It
// caches every byte it receives, reads ahead of the player with parallel
// fetches, and downloads the rest of the file in the background so playback
// no longer depends on the provider keeping up in real time.
type Gateway struct {
	viewer     streamtransport.Viewer
	capability string
	media      protocol.Media
	chunkBytes int64 // largest fetch
	adaptive   int64 // current fetch size, adjusted from measured throughput
	unit       int64 // tracking granularity: unitBytes, or the chunk size if smaller
	path       string
	listener   net.Listener
	server     *http.Server
	transport  *http.Transport
	store      cacheStore
	ctx        context.Context
	cancel     context.CancelFunc

	mu         sync.Mutex
	cond       *sync.Cond
	have       []uint64 // one bit per unit that is fully cached
	haveUnits  int
	units      int
	fetches    map[*fetch]struct{}
	readers    map[*reader]struct{}
	playhead   int64 // latest position any reader asked for
	waiting    int
	backoff    time.Time
	rate       float64 // bytes per second received, smoothed
	rateBytes  int64
	rateAt     time.Time
	closed     bool
	fetchCount atomic.Int64 // upstream requests made, for tests and stats
	received   atomic.Int64 // bytes received from the provider

	foreground chan struct{}
	background chan struct{}
	wake       chan struct{}
	closeOnce  sync.Once
	fillDone   chan struct{}
}

// reader is one player request being served.
type reader struct{ position int64 }

type fetch struct {
	start      int64
	end        int64 // inclusive; may shrink while running
	next       int64 // first byte not yet stored
	marked     int64 // bytes up to here have their units marked
	background bool
	cancel     context.CancelFunc
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
	if cfg.CacheBytes != 0 && (cfg.CacheBytes < chunkBytes || cfg.CacheBytes > maxGatewayCache) {
		return nil, errors.New("stream gateway cache must hold at least one chunk and at most 4 GiB")
	}
	store, err := openCacheStore(cfg, chunkBytes)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("start stream loopback gateway: %w", err)
	}
	token, err := randomPath()
	if err != nil {
		listener.Close()
		store.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	unit := min(unitBytes, chunkBytes)
	// Chunks are whole units, so every fetch ends on a unit boundary.
	chunkBytes = chunkBytes / unit * unit
	units := int((cfg.Media.SizeBytes + unit - 1) / unit)
	gateway := &Gateway{
		viewer: cfg.Viewer, capability: cfg.Capability, media: cfg.Media, chunkBytes: chunkBytes, adaptive: max(min(minAdaptiveChunk, chunkBytes), unit) / unit * unit, unit: unit,
		path: "/" + token, listener: listener, store: store, ctx: ctx, cancel: cancel,
		have: make([]uint64, (units+63)/64), units: units, fetches: make(map[*fetch]struct{}), readers: make(map[*reader]struct{}),
		foreground: make(chan struct{}, foregroundFetches), background: make(chan struct{}, backgroundFetches),
		wake: make(chan struct{}, 1), fillDone: make(chan struct{}),
	}
	gateway.cond = sync.NewCond(&gateway.mu)
	gateway.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return gateway.viewer.Dial(ctx)
		},
		DisableCompression:  true,
		MaxConnsPerHost:     foregroundFetches + backgroundFetches,
		MaxIdleConns:        foregroundFetches + backgroundFetches,
		MaxIdleConnsPerHost: foregroundFetches + backgroundFetches,
		IdleConnTimeout:     30 * time.Second,
	}
	gateway.server = &http.Server{
		Handler: gateway, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
	}
	go func() { _ = gateway.server.Serve(listener) }()
	if store.Complete() && !cfg.DisableBackgroundFill {
		go gateway.fillLoop()
	} else {
		close(gateway.fillDone)
	}
	return gateway, nil
}

func openCacheStore(cfg GatewayConfig, chunkBytes int64) (cacheStore, error) {
	if cfg.CacheBytes > 0 {
		return newMemoryStore(cfg.Media.SizeBytes, cfg.CacheBytes, chunkBytes), nil
	}
	dir := cfg.CacheDir
	if dir == "" {
		if root, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(root, "faro", "streams")
		} else {
			dir = filepath.Join(os.TempDir(), "faro-streams")
		}
	}
	if store, err := newDiskStore(dir, cfg.Media.SizeBytes); err == nil {
		return store, nil
	}
	return newMemoryStore(cfg.Media.SizeBytes, memoryCacheBytes, defaultGatewayChunk), nil
}

func (g *Gateway) URL() string { return "http://" + g.listener.Addr().String() + g.path }

// Progress reports how much of the file is cached and how many bytes have
// arrived from the provider so far.
type Progress struct {
	CachedBytes   int64
	TotalBytes    int64
	ReceivedBytes int64
}

func (g *Gateway) Progress() Progress {
	g.mu.Lock()
	cached := min(int64(g.haveUnits)*g.unit, g.media.SizeBytes)
	g.mu.Unlock()
	return Progress{CachedBytes: cached, TotalBytes: g.media.SizeBytes, ReceivedBytes: g.received.Load()}
}

func (g *Gateway) Close() error {
	var result error
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closed = true
		for f := range g.fetches {
			f.cancel()
		}
		g.cond.Broadcast()
		g.mu.Unlock()
		g.cancel()
		serverErr := g.server.Close()
		g.transport.CloseIdleConnections()
		<-g.fillDone
		result = errors.Join(serverErr, g.viewer.Close(), g.store.Close())
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
	writer.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, g.media.SizeBytes))
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	_ = g.serve(request.Context(), writer, start, end)
}

// serve streams [start, end] to the player, waiting for bytes that are not
// cached yet and keeping the read-ahead window filled.
func (g *Gateway) serve(ctx context.Context, writer io.Writer, start, end int64) error {
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()
	buffer := make([]byte, servePiece)
	current := &reader{position: start}
	g.mu.Lock()
	g.readers[current] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.readers, current)
		g.mu.Unlock()
	}()
	for position := start; position <= end; {
		g.mu.Lock()
		current.position = position
		available := int64(0)
		for {
			if g.closed {
				g.mu.Unlock()
				return net.ErrClosed
			}
			if err := ctx.Err(); err != nil {
				g.mu.Unlock()
				return err
			}
			g.playhead = position
			if available = g.availableLocked(position, end); available > 0 {
				break
			}
			// While the player waits, the link belongs to the byte it needs:
			// stale read-ahead and background downloads stop (what they already
			// received stays cached) and new read-ahead waits until data flows.
			g.releaseLinkLocked()
			g.ensureFetchLocked(position)
			g.waiting++
			g.cond.Wait()
			g.waiting--
		}
		g.scheduleReadAheadLocked(position)
		g.mu.Unlock()
		piece := buffer[:min(available, int64(len(buffer)))]
		read, err := g.store.ReadAt(piece, position)
		if errors.Is(err, errEvicted) || (err != nil && read == 0) {
			// Dropped by the memory cache in the meantime: fetch it again.
			g.forget(position, position+int64(len(piece)))
			continue
		}
		if err := writeGatewayBytes(writer, piece[:read]); err != nil {
			return err
		}
		position += int64(read)
	}
	return nil
}

// availableLocked returns how many bytes from position are readable now:
// fully cached units, or bytes an in-progress fetch has already stored. It
// looks no further than one serve piece ahead.
func (g *Gateway) availableLocked(position, end int64) int64 {
	run := int64(0)
	for unit := position / g.unit; unit < int64(g.units) && g.hasUnitLocked(unit) && run < servePiece; unit++ {
		run = min((unit+1)*g.unit, g.media.SizeBytes) - position
	}
	for f := range g.fetches {
		if f.start <= position && position < f.next {
			run = max(run, f.next-position)
		}
	}
	return min(run, end-position+1)
}

func (g *Gateway) hasUnitLocked(unit int64) bool { return g.have[unit/64]&(1<<(unit%64)) != 0 }

func (g *Gateway) coveredOrFetchingLocked(position int64) bool {
	if g.hasUnitLocked(position / g.unit) {
		return true
	}
	for f := range g.fetches {
		if f.start <= position && position <= f.end {
			return true
		}
	}
	return false
}

// ensureFetchLocked makes sure the byte the player needs is on its way. A
// running fetch that will reach it soon is enough; otherwise a new fetch
// starts at the byte's unit and the old one stops where the new one begins.
func (g *Gateway) ensureFetchLocked(position int64) {
	if time.Now().Before(g.backoff) {
		return
	}
	for f := range g.fetches {
		if f.start <= position && position <= f.end && position-f.next < g.unit*4 {
			return
		}
	}
	start := position / g.unit * g.unit
	for f := range g.fetches {
		if f.start < start && start <= f.end {
			f.end = start - 1
		}
	}
	g.startFetchLocked(start, false)
}

// releaseLinkLocked cancels fetches that compete with a waiting player:
// background downloads, and read-ahead no reader wants.
func (g *Gateway) releaseLinkLocked() {
	for f := range g.fetches {
		if f.background {
			f.cancel()
			continue
		}
		wanted := false
		for r := range g.readers {
			if f.end >= r.position-g.unit && f.start <= min(r.position+readAheadBytes, g.media.SizeBytes) {
				wanted = true
				break
			}
		}
		if !wanted {
			f.cancel()
		}
	}
}

// scheduleReadAheadLocked keeps chunks ahead of the player downloading in
// parallel, and cancels read-ahead no active reader wants any more (a seek
// left it behind). Fetches near another reader are left alone, so two player
// requests at different offsets cannot starve each other.
func (g *Gateway) scheduleReadAheadLocked(position int64) {
	windowEnd := min(position+readAheadBytes, g.media.SizeBytes)
	for f := range g.fetches {
		if f.background {
			continue
		}
		// Between player requests the window around the last playhead stays
		// wanted, so read-ahead carries on while the player is not reading.
		wanted := len(g.readers) == 0 && f.end >= g.playhead-g.unit && f.start <= windowEnd
		for r := range g.readers {
			if f.end >= r.position-g.unit && f.start <= min(r.position+readAheadBytes, g.media.SizeBytes) {
				wanted = true
				break
			}
		}
		if !wanted {
			f.cancel()
		}
	}
	if time.Now().Before(g.backoff) {
		return
	}
	// A fetch about to finish no longer counts, so the next one is already
	// requested when it ends.
	limit, active := g.foregroundLimitLocked(), 0
	for f := range g.fetches {
		if !f.background && f.end-f.next >= pipelineBytes {
			active++
		}
	}
	for cursor := position / g.unit * g.unit; cursor < windowEnd && active < limit; {
		if g.coveredOrFetchingLocked(cursor) {
			cursor += g.unit
			continue
		}
		f := g.startFetchLocked(cursor, false)
		active++
		cursor = f.end + 1
	}
}

// foregroundLimitLocked is how many read-ahead fetches may run at once: one
// sequential stream on a narrow link, parallel fetches on a fast one.
func (g *Gateway) foregroundLimitLocked() int {
	if g.rate >= fastLinkBytes {
		return foregroundFetches
	}
	return 1
}

// observeRateLocked folds newly received bytes into the smoothed link rate.
func (g *Gateway) observeRateLocked(now time.Time) {
	if g.rateAt.IsZero() {
		g.rateAt, g.rateBytes = now, g.received.Load()
		return
	}
	elapsed := now.Sub(g.rateAt)
	if elapsed < 250*time.Millisecond {
		return
	}
	received := g.received.Load()
	instant := float64(received-g.rateBytes) / elapsed.Seconds()
	if g.rate == 0 {
		g.rate = instant
	} else {
		g.rate = g.rate*0.5 + instant*0.5
	}
	g.rateAt, g.rateBytes = now, received
}

// startFetchLocked downloads up to one chunk from start, stopping before
// bytes that are already cached or being fetched.
func (g *Gateway) startFetchLocked(start int64, background bool) *fetch {
	// Parallel fetches use the adaptive size so pieces arrive roughly in
	// order; a single stream and background downloads use whole chunks.
	size := g.chunkBytes
	if !background && g.foregroundLimitLocked() > 1 {
		size = g.adaptive
	}
	end := min(start+size, g.media.SizeBytes) - 1
	for cursor := start + g.unit; cursor <= end; cursor += g.unit {
		if g.coveredOrFetchingLocked(cursor) {
			end = cursor - 1
			break
		}
	}
	ctx, cancel := context.WithCancel(g.ctx)
	f := &fetch{start: start, end: end, next: start, marked: start, background: background, cancel: cancel}
	g.fetches[f] = struct{}{}
	go g.runFetch(ctx, f)
	return f
}

func (g *Gateway) runFetch(ctx context.Context, f *fetch) {
	err := g.download(ctx, f)
	g.mu.Lock()
	delete(g.fetches, f)
	f.cancel()
	if !f.background && !g.closed && err == nil {
		// Keep the read-ahead window full even when no player request is
		// currently asking for data.
		g.scheduleReadAheadLocked(g.playhead)
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		// Waiting readers retry the fetch, but only after a short pause so a
		// busy provider (429 Too Many Requests) is not hammered.
		g.mu.Lock()
		g.backoff = time.Now().Add(fetchRetryDelay)
		g.mu.Unlock()
		time.Sleep(fetchRetryDelay)
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	}
	g.nudgeFill()
}

func (g *Gateway) download(ctx context.Context, f *fetch) error {
	slots := g.foreground
	if f.background {
		slots = g.background
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := g.viewer.WaitForDirect(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	end := f.end
	g.mu.Unlock()
	if end < f.start {
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://faro.media"+upstreamPath, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.capability)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", f.start, end))
	g.fetchCount.Add(1)
	response, err := g.transport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent || response.ContentLength != end-f.start+1 {
		return fmt.Errorf("stream provider returned %s for range %d-%d", response.Status, f.start, end)
	}
	buffer := make([]byte, 64*1024)
	began := time.Now()
	defer func() {
		// Size the next foreground fetches so each takes about chunkDuration
		// at the rate this one achieved.
		elapsed := time.Since(began)
		received := f.next - f.start
		if f.background || received < g.unit || elapsed <= 0 {
			return
		}
		target := int64(float64(received) / elapsed.Seconds() * chunkDuration.Seconds())
		g.mu.Lock()
		g.adaptive = max(min(target, g.chunkBytes), min(minAdaptiveChunk, g.chunkBytes), g.unit) / g.unit * g.unit
		g.mu.Unlock()
	}()
	for {
		g.mu.Lock()
		remaining := f.end - f.next + 1
		g.mu.Unlock()
		if remaining <= 0 {
			return nil // shortened: a newer fetch covers the rest
		}
		read, err := response.Body.Read(buffer[:min(int64(len(buffer)), remaining)])
		if read > 0 {
			evicted, writeErr := g.store.WriteAt(buffer[:read], f.next)
			if writeErr != nil {
				return writeErr
			}
			g.received.Add(int64(read))
			g.mu.Lock()
			f.next += int64(read)
			g.markLocked(f)
			g.observeRateLocked(time.Now())
			for _, span := range evicted {
				g.clearLocked(span[0], span[1])
			}
			g.cond.Broadcast()
			g.mu.Unlock()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// markLocked records the units a fetch has completely stored.
func (g *Gateway) markLocked(f *fetch) {
	for f.marked < g.media.SizeBytes {
		unit := f.marked / g.unit
		unitEnd := min((unit+1)*g.unit, g.media.SizeBytes)
		if f.next < unitEnd {
			return
		}
		if !g.hasUnitLocked(unit) {
			g.have[unit/64] |= 1 << (unit % 64)
			g.haveUnits++
		}
		f.marked = unitEnd
	}
}

func (g *Gateway) clearLocked(start, end int64) {
	for unit := start / g.unit; unit*g.unit < end && unit < int64(g.units); unit++ {
		if g.hasUnitLocked(unit) {
			g.have[unit/64] &^= 1 << (unit % 64)
			g.haveUnits--
		}
	}
}

func (g *Gateway) forget(start, end int64) {
	g.mu.Lock()
	g.clearLocked(start, end)
	g.mu.Unlock()
}

func (g *Gateway) nudgeFill() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// fillLoop downloads the rest of the file while the player does not need the
// bandwidth: forward from the read-ahead window, then from the beginning.
func (g *Gateway) fillLoop() {
	defer close(g.fillDone)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-g.wake:
		case <-ticker.C:
		}
		g.mu.Lock()
		if closed := g.closed; closed || g.haveUnits == g.units {
			g.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		running, foreground := 0, 0
		for f := range g.fetches {
			if f.background {
				running++
			} else {
				foreground++
			}
		}
		// The player and its read-ahead window have the link to themselves;
		// the rest of the file downloads only in bandwidth they leave unused.
		for g.waiting == 0 && foreground == 0 && running < backgroundFetches && !time.Now().Before(g.backoff) {
			start, ok := g.nextGapLocked(min(g.playhead+readAheadBytes, g.media.SizeBytes))
			if !ok {
				break
			}
			g.startFetchLocked(start, true)
			running++
		}
		g.mu.Unlock()
	}
}

// nextGapLocked finds the first unit from `from` (wrapping to the start) that
// is neither cached nor being fetched.
func (g *Gateway) nextGapLocked(from int64) (int64, bool) {
	first := int(from / g.unit)
	for pass := 0; pass < 2; pass++ {
		begin, stop := first, g.units
		if pass == 1 {
			begin, stop = 0, first
		}
		for unit := begin; unit < stop; {
			word := g.have[unit/64] >> (unit % 64)
			if word == ^uint64(0)>>(unit%64) && unit%64 == 0 {
				unit += 64 // whole word cached
				continue
			}
			if word&1 == 1 {
				unit += max(1, bits.TrailingZeros64(^word))
				continue
			}
			if !g.coveredOrFetchingLocked(int64(unit) * g.unit) {
				return int64(unit) * g.unit, true
			}
			unit++
		}
	}
	return 0, false
}

func writeGatewayBytes(writer io.Writer, data []byte) error {
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		return io.ErrShortWrite
	}
	return err
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
