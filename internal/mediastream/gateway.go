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
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	maxGatewayCache = int64(4 * 1024 * 1024 * 1024)
	// Failed fetches back off exponentially from fetchRetryDelay to
	// maxRetryDelay, so a busy provider (429 Too Many Requests) or a broken
	// path is not hammered, and recover by themselves once it heals.
	fetchRetryDelay = 250 * time.Millisecond
	maxRetryDelay   = 4 * time.Second
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

// Transfer health. A connection can stay open while delivering almost
// nothing: a path that degraded mid-transfer, or a TCP connection stuck in
// retransmission backoff. Neither ends on its own, so the gateway watches
// progress and restarts such fetches on fresh connections, resuming from
// the first byte it does not have. Variables so tests can shorten them.
var (
	// stallTimeout restarts a fetch that has received nothing for this long,
	// including while waiting for the response headers.
	stallTimeout = 10 * time.Second
	// slowWindow is how long a busy link is measured before it counts as
	// trickling: below slowFloor, or far below the best rate it has shown.
	// Each restart that does not help doubles the window up to
	// maxSlowWindow, so a link that is simply slow is not reset constantly.
	slowWindow    = 8 * time.Second
	maxSlowWindow = 2 * time.Minute
	slowFloor     = float64(32 * 1024)
	// routeInterval is how often the direct path is verified while data is
	// being fetched; media is never allowed to continue over a relay.
	routeInterval = 5 * time.Second
	healthTick    = time.Second
)

// slowFraction: a busy link delivering less than its best smoothed rate
// divided by this is trickling rather than just busy.
const slowFraction = 16

// minDiskCacheLimit keeps a capped cache far larger than the read-ahead
// window, so blocks being filled are never the least recently used.
var minDiskCacheLimit = int64(1024 * 1024 * 1024)

type GatewayConfig struct {
	Viewer     streamtransport.Viewer
	Capability string
	Media      protocol.Media
	ChunkBytes int64
	// CacheBytes forces a memory cache of this size instead of a disk cache.
	CacheBytes int64
	// CacheDir holds the disk cache; empty uses the user cache directory.
	CacheDir string
	// DiskCacheLimit caps the disk cache. A larger file keeps only the most
	// recently used parts (at least minDiskCacheLimit) and is not downloaded
	// in full; 0 caches the whole file.
	DiskCacheLimit int64
	// DisableBackgroundFill keeps the gateway to what the player asks for
	// plus the read-ahead window.
	DisableBackgroundFill bool
	// OnUpstreamRefused is told, once per provider connection, when the
	// provider refuses the transfer for good: the capability was revoked or
	// expired, or the shared file changed. The gateway has disconnected from
	// that provider by then; Reconnect resumes with another grant.
	OnUpstreamRefused func(error)
	// Logf receives diagnostics about failures, restarts and route changes.
	// It must not block.
	Logf func(format string, args ...any)
}

// Route describes the path to the provider.
type Route string

const (
	// RouteDirect means a peer-to-peer path was verified.
	RouteDirect Route = "direct"
	// RouteUnavailable means no direct path exists at the moment; fetching
	// waits for one to return.
	RouteUnavailable Route = "unavailable"
	// RouteDisconnected means there is no provider connection: the grant was
	// revoked or expired. Cached bytes stay readable.
	RouteDisconnected Route = "disconnected"
)

// Gateway exposes a streamed file to the local player over loopback HTTP. It
// caches every byte it receives, reads ahead of the player with parallel
// fetches, and downloads the rest of the file in the background so playback
// no longer depends on the provider keeping up in real time.
type Gateway struct {
	media      protocol.Media
	chunkBytes int64 // largest fetch
	adaptive   int64 // current fetch size, adjusted from measured throughput
	unit       int64 // tracking granularity: unitBytes, or the chunk size if smaller
	path       string
	listener   net.Listener
	server     *http.Server
	store      cacheStore
	ctx        context.Context
	cancel     context.CancelFunc
	refused    func(error)
	logf       func(string, ...any)

	mu        sync.Mutex
	cond      *sync.Cond
	up        *upstream // nil while disconnected from the provider
	route     Route
	have      []uint64 // one bit per unit that is fully cached
	haveUnits int
	units     int
	fetches   map[*fetch]struct{}
	readers   map[*reader]struct{}
	playhead  int64 // latest position any reader asked for
	waiting   int
	backoff   time.Time
	failures  int // consecutive failed fetches
	lastErr   string
	retry     *time.Timer
	rate      float64 // bytes per second received, smoothed
	peakRate  float64 // best smoothed rate, to tell a trickle from a slow link
	restarts  int     // consecutive trickle restarts that did not help
	rateBytes int64
	rateAt    time.Time
	closed    bool

	fetchCount atomic.Int64 // upstream requests made, for tests and stats
	received   atomic.Int64 // bytes received from the provider

	foreground chan struct{}
	background chan struct{}
	wake       chan struct{}
	closeOnce  sync.Once
	workers    sync.WaitGroup
}

// upstream is one authorized connection to the provider. Reconnect replaces
// it without touching the cache or the player's connection to the gateway.
type upstream struct {
	viewer     streamtransport.Viewer
	capability string
	transport  *http.Transport
	refused    sync.Once
}

func newUpstream(viewer streamtransport.Viewer, capability string) *upstream {
	return &upstream{viewer: viewer, capability: capability, transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return viewer.Dial(ctx)
		},
		DisableCompression:  true,
		MaxConnsPerHost:     foregroundFetches + backgroundFetches,
		MaxIdleConns:        foregroundFetches + backgroundFetches,
		MaxIdleConnsPerHost: foregroundFetches + backgroundFetches,
		IdleConnTimeout:     30 * time.Second,
	}}
}

func (u *upstream) close() error {
	u.transport.CloseIdleConnections()
	return u.viewer.Close()
}

// reader is one player request being served.
type reader struct{ position int64 }

type fetch struct {
	start      int64
	end        int64 // inclusive; may shrink while running
	next       int64 // first byte not yet stored
	marked     int64 // bytes up to here have their units marked
	background bool
	cancel     context.CancelCauseFunc
	// requested is when the request went out; zero while the fetch waits for
	// a slot or a route check. progressAt is when bytes last arrived.
	requested  time.Time
	progressAt time.Time
}

// Reasons a fetch is cancelled. Only errFetchStalled and errNoDirectPath
// mean something went wrong; the others are the scheduler changing its mind.
var (
	errFetchSuperseded = errors.New("fetch is no longer needed")
	errUpstreamChanged = errors.New("provider connection changed")
	errFetchStalled    = errors.New("transfer stalled")
	errNoDirectPath    = errors.New("the direct path to the provider was lost")
	errNoUpstream      = errors.New("not connected to the provider")
	errFetchDone       = errors.New("fetch finished")
)

// UpstreamRefusedError is a provider turning the transfer down for good.
type UpstreamRefusedError struct{ Status int }

func (e *UpstreamRefusedError) Error() string {
	if e.Status == http.StatusGone {
		return "the shared file changed on the provider's device"
	}
	return "the provider no longer allows this transfer"
}

func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	if cfg.Viewer == nil || len(cfg.Capability) < 32 || cfg.Media.SizeBytes <= 0 || cfg.Media.SizeBytes > protocol.MaxStreamBytes || !validFileFingerprint(cfg.Media.Fingerprint) {
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
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	gateway := &Gateway{
		media: cfg.Media, chunkBytes: chunkBytes, adaptive: max(min(minAdaptiveChunk, chunkBytes), unit) / unit * unit, unit: unit,
		path: "/" + token + "/" + gatewayFileName(cfg.Media.Title), listener: listener, store: store, ctx: ctx, cancel: cancel,
		refused: cfg.OnUpstreamRefused, logf: logf, up: newUpstream(cfg.Viewer, cfg.Capability), route: RouteDirect,
		have: make([]uint64, (units+63)/64), units: units, fetches: make(map[*fetch]struct{}), readers: make(map[*reader]struct{}),
		foreground: make(chan struct{}, foregroundFetches), background: make(chan struct{}, backgroundFetches),
		wake: make(chan struct{}, 1),
	}
	gateway.cond = sync.NewCond(&gateway.mu)
	gateway.server = &http.Server{
		Handler: gateway, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
	}
	go func() { _ = gateway.server.Serve(listener) }()
	gateway.workers.Add(2)
	go gateway.healthLoop()
	go gateway.routeLoop()
	if store.Complete() && !cfg.DisableBackgroundFill {
		gateway.workers.Add(1)
		go gateway.fillLoop()
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
	if limit := cfg.DiskCacheLimit; limit > 0 && limit < cfg.Media.SizeBytes {
		if store, err := newDiskBlockStore(dir, cfg.Media.SizeBytes, max(limit, minDiskCacheLimit), chunkBytes); err == nil {
			return store, nil
		}
	} else if store, err := newDiskStore(dir, cfg.Media.SizeBytes); err == nil {
		return store, nil
	}
	return newMemoryStore(cfg.Media.SizeBytes, memoryCacheBytes, defaultGatewayChunk), nil
}

// gatewayFileName ends the gateway URL with the file's name: players title
// media without embedded metadata by the last URL segment, so they show
// "Movie.mkv" rather than an opaque token.
func gatewayFileName(title string) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(title, "\\", "/")))
	if name == "" || name == "." || name == "/" {
		name = "media"
	}
	return url.PathEscape(name)
}

func (g *Gateway) URL() string { return "http://" + g.listener.Addr().String() + g.path }

// Progress reports how much of the file is cached, how many bytes have
// arrived from the provider so far, and the state of the path to it.
type Progress struct {
	CachedBytes   int64
	TotalBytes    int64
	ReceivedBytes int64
	Complete      bool
	Route         Route
	// Error is the latest fetch failure, cleared by the next success.
	Error string
}

func (g *Gateway) Progress() Progress {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Progress{
		CachedBytes: min(int64(g.haveUnits)*g.unit, g.media.SizeBytes), TotalBytes: g.media.SizeBytes,
		ReceivedBytes: g.received.Load(), Complete: g.haveUnits == g.units, Route: g.route, Error: g.lastErr,
	}
}

// Complete reports whether every byte of the file is cached, so playback no
// longer needs the provider.
func (g *Gateway) Complete() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.haveUnits == g.units
}

// CachedRanges reports the cached parts of the file as [start, end) fractions
// of its size, in order. Nearby ranges are merged so there are at most limit.
func (g *Gateway) CachedRanges(limit int) [][2]float64 {
	g.mu.Lock()
	var runs [][2]int64
	for unit := int64(0); unit < int64(g.units); unit++ {
		if !g.hasUnitLocked(unit) {
			continue
		}
		if last := len(runs) - 1; last >= 0 && runs[last][1] == unit {
			runs[last][1] = unit + 1
		} else {
			runs = append(runs, [2]int64{unit, unit + 1})
		}
	}
	units := float64(g.units)
	g.mu.Unlock()
	if limit < 1 {
		limit = 1
	}
	if len(runs) > limit {
		// Merge across the smallest gaps first.
		gaps := make([]int64, 0, len(runs)-1)
		for index := 1; index < len(runs); index++ {
			gaps = append(gaps, runs[index][0]-runs[index-1][1])
		}
		slices.Sort(gaps)
		threshold := gaps[len(runs)-limit-1]
		merged := runs[:1]
		for _, run := range runs[1:] {
			if last := len(merged) - 1; run[0]-merged[last][1] <= threshold {
				merged[last][1] = run[1]
			} else {
				merged = append(merged, run)
			}
		}
		runs = merged
	}
	ranges := make([][2]float64, len(runs))
	for index, run := range runs {
		ranges[index] = [2]float64{float64(run[0]) / units, float64(run[1]) / units}
	}
	return ranges
}

func (g *Gateway) Close() error {
	var result error
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closed = true
		up := g.up
		g.up = nil
		g.cancelFetchesLocked(func(*fetch) bool { return true }, errFetchSuperseded)
		if g.retry != nil {
			g.retry.Stop()
		}
		g.cond.Broadcast()
		g.mu.Unlock()
		g.cancel()
		serverErr := g.server.Close()
		g.workers.Wait()
		var upstreamErr error
		if up != nil {
			upstreamErr = up.close()
		}
		g.mu.Lock()
		store := g.store
		g.mu.Unlock()
		result = errors.Join(serverErr, upstreamErr, store.Close())
	})
	return result
}

// Disconnect drops the provider connection, for a grant that was revoked or
// expired. Cached bytes stay readable; a player asking for anything else
// waits until Reconnect supplies a new connection or the gateway closes.
func (g *Gateway) Disconnect() {
	g.mu.Lock()
	up := g.up
	g.up = nil
	if !g.closed {
		g.route = RouteDisconnected
	}
	g.cancelFetchesLocked(func(*fetch) bool { return true }, errUpstreamChanged)
	g.cond.Broadcast()
	g.mu.Unlock()
	if up != nil {
		up.close()
	}
}

// Reconnect continues the transfer through a new provider connection, after
// the previous grant ended. The viewer must already have a verified direct
// path. The cache and the player's connection are kept, so playback resumes
// where it was.
func (g *Gateway) Reconnect(viewer streamtransport.Viewer, capability string) error {
	if viewer == nil || len(capability) < 32 {
		return errors.New("stream gateway reconnection is invalid")
	}
	next := newUpstream(viewer, capability)
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		next.close()
		return net.ErrClosed
	}
	previous := g.up
	g.up, g.route = next, RouteDirect
	g.failures, g.backoff, g.lastErr, g.restarts = 0, time.Time{}, "", 0
	g.cancelFetchesLocked(func(*fetch) bool { return true }, errUpstreamChanged)
	g.scheduleReadAheadLocked(g.playhead)
	g.cond.Broadcast()
	g.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	g.logf("stream reconnected to a provider")
	g.nudgeFill()
	return nil
}

func (g *Gateway) cancelFetchesLocked(match func(*fetch) bool, cause error) int {
	cancelled := 0
	for f := range g.fetches {
		if match(f) {
			f.cancel(cause)
			cancelled++
		}
	}
	return cancelled
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if decoded, err := url.PathUnescape(g.path); err != nil || request.URL.Path != decoded {
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
		store := g.store
		evictions := store.Evictions()
		g.mu.Unlock()
		piece := buffer[:min(available, int64(len(buffer)))]
		read, err := store.ReadAt(piece, position)
		if errors.Is(err, errEvicted) || (err != nil && read == 0) {
			// Evicted by a bounded cache, or the cache was replaced, in the
			// meantime: fetch it again.
			g.forget(position, position+int64(len(piece)))
			continue
		}
		if store.Evictions() != evictions {
			// The bounded cache evicted a block meanwhile; what was read may
			// come from a block recreated since. Check again what is cached.
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
	if !g.canFetchLocked() {
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

// canFetchLocked reports whether new fetches may start: there is a provider
// connection and no failure backoff is in effect.
func (g *Gateway) canFetchLocked() bool {
	return g.up != nil && !g.closed && !time.Now().Before(g.backoff)
}

// releaseLinkLocked cancels fetches that compete with a waiting player:
// background downloads, and read-ahead no reader wants.
func (g *Gateway) releaseLinkLocked() {
	g.cancelFetchesLocked(func(f *fetch) bool { return f.background || !g.wantedByReaderLocked(f) }, errFetchSuperseded)
}

func (g *Gateway) wantedByReaderLocked(f *fetch) bool {
	for r := range g.readers {
		if f.end >= r.position-g.unit && f.start <= min(r.position+readAheadBytes, g.media.SizeBytes) {
			return true
		}
	}
	return false
}

// scheduleReadAheadLocked keeps chunks ahead of the player downloading in
// parallel, and cancels read-ahead no active reader wants any more (a seek
// left it behind). Fetches near another reader are left alone, so two player
// requests at different offsets cannot starve each other.
func (g *Gateway) scheduleReadAheadLocked(position int64) {
	windowEnd := min(position+readAheadBytes, g.media.SizeBytes)
	g.cancelFetchesLocked(func(f *fetch) bool {
		if f.background {
			return false
		}
		// Between player requests the window around the last playhead stays
		// wanted, so read-ahead carries on while the player is not reading.
		idleWanted := len(g.readers) == 0 && f.end >= g.playhead-g.unit && f.start <= windowEnd
		return !idleWanted && !g.wantedByReaderLocked(f)
	}, errFetchSuperseded)
	if !g.canFetchLocked() {
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
	g.peakRate = max(g.peakRate, g.rate)
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
	ctx, cancel := context.WithCancelCause(g.ctx)
	f := &fetch{start: start, end: end, next: start, marked: start, background: background, cancel: cancel}
	g.fetches[f] = struct{}{}
	g.workers.Add(1)
	go g.runFetch(ctx, f)
	return f
}

func (g *Gateway) runFetch(ctx context.Context, f *fetch) {
	defer g.workers.Done()
	err := g.download(ctx, f)
	// Why the fetch ended must be read before its context is released.
	cause := context.Cause(ctx)
	f.cancel(errFetchDone)
	g.mu.Lock()
	delete(g.fetches, f)
	if !g.closed {
		g.fetchEndedLocked(f, err, cause)
		// Keep the read-ahead window full even when no player request is
		// currently asking for data; after a failure this waits out the
		// backoff, and the retry timer starts it again.
		g.scheduleReadAheadLocked(g.playhead)
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	g.nudgeFill()
}

// fetchEndedLocked classifies how a fetch ended. Fetches the scheduler no
// longer wanted are not failures; a stalled one is restarted at once on a
// fresh connection; anything else backs off before the next attempt, and a
// provider that refuses the transfer is disconnected.
func (g *Gateway) fetchEndedLocked(f *fetch, err, cause error) {
	switch {
	case err == nil:
		g.failures, g.lastErr = 0, ""
	case cause == errFetchSuperseded || cause == errUpstreamChanged || errors.Is(err, errNoUpstream) || errors.Is(err, errStoreReplaced):
	case cause == errFetchStalled:
	default:
		if cause == errNoDirectPath {
			err = cause
		}
		var refused *UpstreamRefusedError
		if errors.As(err, &refused) {
			g.refuseLocked(err)
			return
		}
		g.failures++
		g.lastErr = err.Error()
		delay := min(fetchRetryDelay<<min(g.failures-1, 8), maxRetryDelay)
		g.backoff = time.Now().Add(delay)
		if g.retry != nil {
			g.retry.Stop()
		}
		g.retry = time.AfterFunc(delay, g.retryFetches)
		if g.failures&(g.failures-1) == 0 {
			g.logf("stream fetch %d-%d failed (attempt %d, retrying in %s): %v", f.start, f.end, g.failures, delay, err)
		}
	}
}

// retryFetches resumes fetching once a failure backoff has passed: waiting
// players ask again, and read-ahead continues even if none is reading.
func (g *Gateway) retryFetches() {
	g.mu.Lock()
	if !g.closed {
		g.scheduleReadAheadLocked(g.playhead)
		g.cond.Broadcast()
	}
	g.mu.Unlock()
	g.nudgeFill()
}

// refuseLocked disconnects from a provider that turned the transfer down
// for good and tells the owner, once per provider connection.
func (g *Gateway) refuseLocked(err error) {
	up := g.up
	if up == nil {
		return
	}
	g.up, g.route, g.lastErr = nil, RouteDisconnected, err.Error()
	g.cancelFetchesLocked(func(*fetch) bool { return true }, errUpstreamChanged)
	g.logf("stream provider refused the transfer: %v", err)
	go func() {
		up.close()
		up.refused.Do(func() {
			if g.refused != nil {
				g.refused(err)
			}
		})
	}()
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
	g.mu.Lock()
	up := g.up
	g.mu.Unlock()
	if up == nil {
		return errNoUpstream
	}
	if err := up.viewer.WaitForDirect(ctx); err != nil {
		if ctx.Err() == nil {
			g.setRoute(up, RouteUnavailable)
		}
		return err
	}
	// clearLocked may move f.start while the fetch runs; the request covers
	// the range as it was when the fetch began.
	g.mu.Lock()
	if g.up == up && g.route == RouteUnavailable {
		g.route = RouteDirect
		g.logf("stream direct path restored")
	}
	start, end := f.start, f.end
	f.requested, f.progressAt = time.Now(), time.Now()
	g.mu.Unlock()
	if end < start {
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://faro.media"+upstreamPath, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+up.capability)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	g.fetchCount.Add(1)
	response, err := up.transport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusPartialContent:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return &UpstreamRefusedError{Status: response.StatusCode}
	default:
		return fmt.Errorf("stream provider returned %s for range %d-%d", response.Status, start, end)
	}
	if response.ContentLength != end-start+1 {
		return fmt.Errorf("stream provider returned %d bytes for range %d-%d", response.ContentLength, start, end)
	}
	buffer := make([]byte, 64*1024)
	began := time.Now()
	defer func() {
		// Size the next foreground fetches so each takes about chunkDuration
		// at the rate this one achieved.
		elapsed := time.Since(began)
		received := f.next - start
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
		store := g.store
		g.mu.Unlock()
		if remaining <= 0 {
			return nil // shortened: a newer fetch covers the rest
		}
		read, err := response.Body.Read(buffer[:min(int64(len(buffer)), remaining)])
		if read > 0 {
			// A bounded cache evicts while writing. The write and recording
			// what it evicted happen under one lock, so no reader and no other
			// fetch can see a unit as cached after its block was dropped.
			evicting := !store.Complete()
			if evicting {
				g.mu.Lock()
			}
			evicted, writeErr := store.WriteAt(buffer[:read], f.next)
			if !evicting {
				g.mu.Lock()
			}
			if writeErr != nil {
				g.replaceFailedStoreLocked(store, writeErr)
				g.mu.Unlock()
				return writeErr
			}
			g.received.Add(int64(read))
			if g.store != store {
				g.mu.Unlock()
				return errStoreReplaced
			}
			now := time.Now()
			f.next += int64(read)
			f.progressAt = now
			for _, span := range evicted {
				g.clearLocked(span[0], span[1])
			}
			g.markLocked(f)
			g.observeRateLocked(now)
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

func (g *Gateway) setRoute(up *upstream, route Route) {
	g.mu.Lock()
	if g.up == up && g.route != route {
		g.route = route
		g.logf("stream route is now %s", route)
	}
	g.mu.Unlock()
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
	// A running fetch may already have written into the dropped range: those
	// bytes are no longer readable through it, and its units there must not be
	// marked as cached later.
	for f := range g.fetches {
		if start < f.next && f.start < end {
			f.start = min(max(f.start, end), f.next)
			f.marked = max(f.marked, (end+g.unit-1)/g.unit*g.unit)
		}
	}
}

var errStoreReplaced = errors.New("stream cache was replaced")

// replaceFailedStoreLocked switches to a memory cache after a disk cache
// write fails, typically because the disk filled up after the stream began.
// Everything cached on disk is dropped and running fetches restart.
func (g *Gateway) replaceFailedStoreLocked(failed cacheStore, cause error) {
	if g.store != failed || g.closed {
		return
	}
	if bounded, ok := failed.(*blockStore); ok && !bounded.onDisk() {
		return
	}
	g.logf("stream disk cache failed, continuing in memory: %v", cause)
	g.store = newMemoryStore(g.media.SizeBytes, memoryCacheBytes, g.chunkBytes)
	clear(g.have)
	g.haveUnits = 0
	g.cancelFetchesLocked(func(*fetch) bool { return true }, errFetchSuperseded)
	g.cond.Broadcast()
	go failed.Close()
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

// healthLoop restarts transfers that stopped making progress. A fetch that
// receives nothing for stallTimeout is restarted alone. A busy link whose
// throughput over a whole window stays below slowFloor, or far below the
// best rate it has shown, restarts every fetch on fresh connections: a
// degraded path or a connection stuck in retransmission backoff does not
// recover by itself, and a new connection resumes from the first missing
// byte. Restarts that do not help widen the window and lower the expected
// rate, so a link that is simply slow settles instead of being reset.
func (g *Gateway) healthLoop() {
	defer g.workers.Done()
	ticker := time.NewTicker(healthTick)
	defer ticker.Stop()
	var windowStart time.Time
	var windowBytes int64
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		g.mu.Lock()
		busy := false
		for f := range g.fetches {
			if f.requested.IsZero() {
				continue
			}
			busy = true
			if idle := now.Sub(f.progressAt); idle >= stallTimeout {
				g.logf("stream fetch %d-%d received nothing for %s; restarting it", f.start, f.end, idle.Round(time.Second))
				f.cancel(errFetchStalled)
			}
		}
		switch {
		case !busy:
			// Only time spent fetching says anything about the link.
			windowStart = time.Time{}
		case windowStart.IsZero():
			windowStart, windowBytes = now, g.received.Load()
		case now.Sub(windowStart) >= min(slowWindow<<min(g.restarts, 8), maxSlowWindow):
			rate := float64(g.received.Load()-windowBytes) / now.Sub(windowStart).Seconds()
			threshold := max(slowFloor, g.peakRate/slowFraction)
			if rate < threshold {
				restarted := g.cancelFetchesLocked(func(f *fetch) bool { return !f.requested.IsZero() }, errFetchStalled)
				g.logf("stream link delivered %.0f B/s, expected at least %.0f B/s; restarting %d fetches", rate, threshold, restarted)
				g.restarts++
				g.peakRate /= 2
				if g.up != nil {
					g.up.transport.CloseIdleConnections()
				}
			} else {
				g.restarts = 0
			}
			windowStart = time.Time{}
		}
		g.mu.Unlock()
	}
}

// routeLoop re-verifies the direct path while data is being fetched. The
// path can fall back to a relay after a transfer began; when it does, the
// transfer stops and waits for a direct path rather than continuing over
// the relay.
func (g *Gateway) routeLoop() {
	defer g.workers.Done()
	ticker := time.NewTicker(routeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		g.mu.Lock()
		up, busy := g.up, false
		for f := range g.fetches {
			busy = busy || !f.requested.IsZero()
		}
		g.mu.Unlock()
		if up == nil || !busy {
			continue
		}
		ctx, cancel := context.WithTimeout(g.ctx, 15*time.Second)
		err := up.viewer.WaitForDirect(ctx)
		cancel()
		if g.ctx.Err() != nil {
			return
		}
		if err != nil {
			g.setRoute(up, RouteUnavailable)
			g.mu.Lock()
			if g.up == up {
				g.cancelFetchesLocked(func(f *fetch) bool { return !f.requested.IsZero() }, errNoDirectPath)
			}
			g.mu.Unlock()
		} else {
			g.setRoute(up, RouteDirect)
		}
	}
}

// fillLoop downloads the rest of the file while the player does not need the
// bandwidth: forward from the read-ahead window, then from the beginning.
func (g *Gateway) fillLoop() {
	defer g.workers.Done()
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
		if closed := g.closed; closed || g.haveUnits == g.units || !g.store.Complete() {
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
		for g.waiting == 0 && foreground == 0 && running < backgroundFetches && g.canFetchLocked() {
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
