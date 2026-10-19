package mpv

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/player/discovery"
)

const requestTimeout = 3 * time.Second

// wedgedWriteTimeout retires a player whose control channel has stopped
// accepting writes for this long: mpv reads its IPC socket on its own
// thread, so a write that cannot complete means the player is gone or hung.
const wedgedWriteTimeout = 10 * time.Second

type response struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	RequestID int64           `json:"request_id"`
	Event     string          `json:"event"`
	Name      string          `json:"name"`
	Reason    string          `json:"reason"`
	FileError string          `json:"file_error"`
}

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 4 * 1024
	b.data = append(b.data, data...)
	if len(b.data) > limit {
		b.data = append([]byte(nil), b.data[len(b.data)-limit:]...)
	}
	return len(data), nil
}

func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

type MPV struct {
	process   *exec.Cmd
	stream    io.ReadWriteCloser
	cleanup   func()
	killTree  func()
	writes    chan write
	wedged    time.Duration
	pendingMu sync.Mutex
	pending   map[int64]chan response
	requestID atomic.Int64
	openMu    sync.Mutex
	loadMu    sync.Mutex
	loaded    chan error

	stateMu sync.RWMutex
	state   player.State
	events  *player.EventQueue
	done    chan struct{}
	once    sync.Once
}

// write is one line for the control channel; writeLoop owns all writes.
type write struct {
	data    []byte
	written chan error
}

// newMPV wraps an established control channel. Start launches the process
// and connects first; tests use it directly with an in-memory stream.
func newMPV(stream io.ReadWriteCloser, wedged time.Duration) *MPV {
	m := &MPV{
		stream: stream, cleanup: func() {}, killTree: func() {}, wedged: wedged,
		writes: make(chan write), pending: make(map[int64]chan response), events: player.NewEventQueue(),
		done: make(chan struct{}), state: player.State{Paused: true, Rate: 1, ObservedAt: time.Now()},
	}
	go m.writeLoop()
	return m
}

func Start(ctx context.Context, cfg Config, initialSource string) (*MPV, error) {
	executable, err := cfg.executable()
	if err != nil {
		return nil, err
	}
	// The random part keeps another local process from predicting the name
	// and creating the pipe before mpv does.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d-%x", time.Now().UnixNano(), nonce)
	ipcPath, cleanup, err := ipcAddress(id)
	if err != nil {
		return nil, err
	}
	// Player lifetime is owned by MPV.Close rather than exec.CommandContext.
	// In particular, iina-cli is a wrapper around the real IINA process; a
	// context cancellation would SIGKILL only the wrapper and orphan IINA.
	command := exec.Command(discovery.Resolve(executable), cfg.arguments(ipcPath, initialSource)...)
	if cfg.Profile == ProfileMPVNet {
		cleanup, err = isolateMPVNet(command, cleanup)
		if err != nil {
			return nil, err
		}
	}
	output := &boundedOutput{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start %s: %w", cfg.Profile, err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	killTree, err := assignProcessCleanup(command.Process)
	if err != nil {
		killTree = func() { _ = command.Process.Kill() }
	}
	stream, err := waitForIPC(ctx, processDone, ipcPath, output)
	if err != nil {
		killTree()
		cleanup()
		return nil, fmt.Errorf("start %s control channel: %w", cfg.Profile, err)
	}
	instance := newMPV(stream, wedgedWriteTimeout)
	instance.process, instance.cleanup, instance.killTree = command, cleanup, killTree
	go instance.readLoop()
	go instance.waitLoop(processDone, output)
	for _, property := range []string{"pause", "time-pos", "speed", "duration", "media-title", "path", "chapter-list", "paused-for-cache", "idle-active"} {
		if err := instance.command(ctx, []any{"observe_property", instance.requestID.Add(1), property}, nil); err != nil {
			instance.Close()
			return nil, fmt.Errorf("observe mpv property %s: %w", property, err)
		}
	}
	return instance, nil
}

func isolateMPVNet(command *exec.Cmd, cleanup func()) (func(), error) {
	// Isolate Faro from mpv.net's normal mpv.conf/mpvnet.conf. In particular,
	// a saved single-process or IPC setting must not redirect Faro's commands
	// to a different player window.
	configDirectory, err := os.MkdirTemp("", "faro-mpvnet-*")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create isolated mpv.net profile: %w", err)
	}
	command.Env = withEnvironmentValue(os.Environ(), "MPVNET_HOME", configDirectory)
	return func() {
		cleanup()
		_ = os.RemoveAll(configDirectory)
	}, nil
}

func withEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.EqualFold(strings.SplitN(entry, "=", 2)[0]+"=", prefix) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}

func waitForIPC(ctx context.Context, processDone <-chan error, path string, output *boundedOutput) (io.ReadWriteCloser, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		stream, err := dialIPC(path)
		if err == nil {
			return stream, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case processErr := <-processDone:
			return nil, withPlayerOutput(fmt.Errorf("player exited before IPC became ready: %w", processErr), output)
		case <-timeout.C:
			return nil, withPlayerOutput(errors.New("timed out waiting for player IPC"), output)
		case <-ticker.C:
		}
	}
}

func (m *MPV) Capabilities() player.Capabilities {
	return player.Capabilities{PlaybackRate: true, URLs: true}
}

func (m *MPV) State(ctx context.Context) (player.State, error) {
	select {
	case <-ctx.Done():
		return player.State{}, ctx.Err()
	case <-m.done:
		return player.State{}, errors.New("mpv is closed")
	default:
	}
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	state := m.state
	state.Chapters = append([]player.Chapter(nil), m.state.Chapters...)
	return state, nil
}

func (m *MPV) SetPaused(ctx context.Context, paused bool) error {
	return m.command(ctx, []any{"set_property", "pause", paused}, nil)
}
func (m *MPV) Seek(ctx context.Context, seconds float64) error {
	return m.command(ctx, []any{"seek", math.Max(0, seconds), "absolute+exact"}, nil)
}
func (m *MPV) SetRate(ctx context.Context, rate float64) error {
	return m.command(ctx, []any{"set_property", "speed", rate}, nil)
}
func (m *MPV) Open(ctx context.Context, source string) error {
	m.openMu.Lock()
	defer m.openMu.Unlock()
	m.setMediaTitle(ctx, "")
	return m.openSource(ctx, source)
}

// setMediaTitle sets or (with "") clears force-media-title, which mpv shows
// in its window title and OSD and reports as media-title. Resolved YouTube
// streams would otherwise be titled by their extracted URL. A property works
// on every mpv version, unlike per-file loadfile options.
func (m *MPV) setMediaTitle(ctx context.Context, title string) {
	_ = m.command(ctx, []any{"set_property", "force-media-title", title}, nil)
}

func (m *MPV) OpenResolved(ctx context.Context, stream player.ResolvedStream) error {
	m.openMu.Lock()
	defer m.openMu.Unlock()

	primary := stream.VideoURL
	if primary == "" {
		primary = stream.CombinedURL
	}
	if primary == "" {
		return errors.New("resolved stream has no video URL")
	}
	m.setMediaTitle(ctx, strings.TrimSpace(stream.Title))
	if err := m.openSource(ctx, primary); err != nil {
		if stream.CombinedURL != "" && primary != stream.CombinedURL {
			return m.openSource(ctx, stream.CombinedURL)
		}
		return err
	}
	if stream.VideoURL != "" && stream.AudioURL != "" {
		if err := m.command(ctx, []any{"audio-add", stream.AudioURL, "select"}, nil); err != nil {
			if stream.CombinedURL != "" {
				return m.openSource(ctx, stream.CombinedURL)
			}
			return fmt.Errorf("load resolved audio stream: %w", err)
		}
	}
	return nil
}

func (m *MPV) openSource(ctx context.Context, source string) error {
	loaded := make(chan error, 1)
	m.loadMu.Lock()
	m.loaded = loaded
	m.loadMu.Unlock()
	if err := m.command(ctx, []any{"loadfile", source, "replace"}, nil); err != nil {
		m.clearLoad(loaded)
		return err
	}
	select {
	case loadErr := <-loaded:
		if loadErr != nil {
			return loadErr
		}
		select {
		case <-m.done:
			return errors.New("mpv closed while loading media")
		default:
		}
		var loadedPath string
		if err := m.command(ctx, []any{"get_property", "path"}, &loadedPath); err != nil {
			return fmt.Errorf("verify loaded media: %w", err)
		}
		if err := verifyLoadedSource(source, loadedPath); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		m.clearLoad(loaded)
		return ctx.Err()
	case <-time.After(15 * time.Second):
		m.clearLoad(loaded)
		return errors.New("timed out waiting for mpv to load media")
	case <-m.done:
		return errors.New("mpv closed while loading media")
	}
}

func verifyLoadedSource(requested, loaded string) error {
	loaded = strings.TrimSpace(loaded)
	if loaded == "" {
		return errors.New("player reported file-loaded but has no media path")
	}
	// Network players may expose the post-redirect URL. file-loaded plus a
	// nonempty path is the strongest stable check available for those sources.
	if strings.Contains(requested, "://") {
		return nil
	}
	requestedInfo, requestedErr := os.Stat(requested)
	loadedInfo, loadedErr := os.Stat(loaded)
	if requestedErr == nil && loadedErr == nil && os.SameFile(requestedInfo, loadedInfo) {
		return nil
	}
	requestedPath, requestedAbsErr := filepath.Abs(requested)
	loadedPath, loadedAbsErr := filepath.Abs(loaded)
	if requestedAbsErr == nil && loadedAbsErr == nil {
		if filepath.Clean(requestedPath) == filepath.Clean(loadedPath) ||
			(runtime.GOOS == "windows" && strings.EqualFold(filepath.Clean(requestedPath), filepath.Clean(loadedPath))) {
			return nil
		}
	}
	return fmt.Errorf("player loaded %q instead of requested media %q", loaded, requested)
}

func (m *MPV) clearLoad(target chan error) {
	m.loadMu.Lock()
	if m.loaded == target {
		m.loaded = nil
	}
	m.loadMu.Unlock()
}

// completeLoad answers the pending Open, if there is one, and reports
// whether there was.
func (m *MPV) completeLoad(err error) bool {
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	loaded := m.loaded
	m.loaded = nil
	if loaded != nil {
		loaded <- err
	}
	return loaded != nil
}
func (m *MPV) Events() <-chan player.Event { return m.events.Events() }

// command sends one IPC request and waits for its reply. The whole exchange,
// including waiting for the control channel to accept the write, is bounded
// by ctx and requestTimeout; a cancelled operation never reaches the player.
func (m *MPV) command(ctx context.Context, command []any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := m.requestID.Add(1)
	request := map[string]any{"command": command, "request_id": id}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	responseChannel := make(chan response, 1)
	m.pendingMu.Lock()
	m.pending[id] = responseChannel
	m.pendingMu.Unlock()
	defer func() { m.pendingMu.Lock(); delete(m.pending, id); m.pendingMu.Unlock() }()
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	if err := m.send(ctx, append(data, '\n'), timer.C); err != nil {
		return err
	}
	select {
	case reply := <-responseChannel:
		if reply.Error != "" && reply.Error != "success" {
			return errors.New(reply.Error)
		}
		if result != nil && len(reply.Data) != 0 && string(reply.Data) != "null" {
			if err := json.Unmarshal(reply.Data, result); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("mpv request timed out")
	case <-m.done:
		return errors.New("mpv is closed")
	}
}

// send hands data to writeLoop. It gives up when ctx ends or timeout fires
// before the control channel accepts the write.
func (m *MPV) send(ctx context.Context, data []byte, timeout <-chan time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := write{data: data, written: make(chan error, 1)}
	select {
	case m.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout:
		return errors.New("mpv is not accepting commands")
	case <-m.done:
		return errors.New("mpv is closed")
	}
	select {
	case err := <-request.written:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout:
		return errors.New("mpv is not accepting commands")
	case <-m.done:
		return errors.New("mpv is closed")
	}
}

// writeLoop performs every write to the control channel, one at a time. A
// write that cannot complete for m.wedged retires the player:
// closing it unblocks the write and every command waiting behind it.
func (m *MPV) writeLoop() {
	for {
		select {
		case request := <-m.writes:
			watchdog := time.AfterFunc(m.wedged, func() { m.Close() })
			_, err := m.stream.Write(request.data)
			watchdog.Stop()
			request.written <- err
			if err != nil {
				m.Close()
				return
			}
		case <-m.done:
			return
		}
	}
}

func (m *MPV) readLoop() {
	scanner := bufio.NewScanner(m.stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var message response
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if message.RequestID != 0 {
			m.pendingMu.Lock()
			pending := m.pending[message.RequestID]
			m.pendingMu.Unlock()
			if pending != nil {
				pending <- message
			}
			continue
		}
		m.handleEvent(message)
	}
	if err := scanner.Err(); err != nil {
		m.emit(player.Event{Kind: player.EventClosed, Err: err})
	}
	m.Close()
}

func (m *MPV) handleEvent(message response) {
	switch message.Event {
	case "property-change":
		m.applyProperty(message.Name, message.Data)
	case "file-loaded":
		m.completeLoad(nil)
	case "end-file":
		if message.Reason != "error" {
			return
		}
		detail := strings.TrimSpace(message.FileError)
		if detail == "" {
			detail = "unknown playback error"
		}
		// A failure while loading answers the pending Open. One after the
		// file loaded (a network source that broke off mid-playback) has no
		// caller waiting, so it is reported as an event.
		if !m.completeLoad(fmt.Errorf("player could not load media: %s", detail)) {
			state, _ := m.State(context.Background())
			m.emit(player.Event{Kind: player.EventFailed, State: state, Err: fmt.Errorf("playback failed: %s", detail)})
		}
	}
}

func (m *MPV) applyProperty(name string, raw any) {
	state := m.updateProperty(name, raw)
	kind := player.EventState
	if name == "media-title" || name == "path" || name == "duration" || name == "chapter-list" || name == "idle-active" {
		kind = player.EventMedia
	}
	m.emit(player.Event{Kind: kind, State: state})
}

func (m *MPV) updateProperty(name string, raw any) player.State {
	data, ok := raw.(json.RawMessage)
	if !ok {
		data, _ = json.Marshal(raw)
	}
	m.stateMu.Lock()
	if name == "pause" || name == "speed" || name == "paused-for-cache" {
		now := time.Now()
		if !m.state.Paused && !m.state.Buffering && !m.state.ObservedAt.IsZero() {
			m.state.PositionSeconds += now.Sub(m.state.ObservedAt).Seconds() * m.state.Rate
		}
		m.state.ObservedAt = now
	}
	// mpv reports a property that became unavailable (nothing is loaded any
	// more) without data. Media fields are reset before decoding, so an old
	// value cannot linger.
	switch name {
	case "paused-for-cache":
		m.state.Buffering = false
		_ = json.Unmarshal(data, &m.state.Buffering)
	case "pause":
		_ = json.Unmarshal(data, &m.state.Paused)
	case "time-pos":
		m.state.PositionSeconds = 0
		_ = json.Unmarshal(data, &m.state.PositionSeconds)
	case "speed":
		_ = json.Unmarshal(data, &m.state.Rate)
	case "duration":
		m.state.DurationSeconds = 0
		_ = json.Unmarshal(data, &m.state.DurationSeconds)
	case "media-title":
		m.state.Title = ""
		_ = json.Unmarshal(data, &m.state.Title)
	case "path":
		m.state.Source = ""
		_ = json.Unmarshal(data, &m.state.Source)
	case "chapter-list":
		m.state.Chapters = nil
		_ = json.Unmarshal(data, &m.state.Chapters)
	case "idle-active":
		// Idle means nothing is loaded: the file ended, failed or was
		// stopped. Nothing advances until the next file loads.
		var idle bool
		if json.Unmarshal(data, &idle) == nil && idle {
			m.state.Source, m.state.Title, m.state.Chapters = "", "", nil
			m.state.PositionSeconds, m.state.DurationSeconds = 0, 0
			m.state.Buffering = false
		}
	}
	if name == "time-pos" || m.state.ObservedAt.IsZero() {
		m.state.ObservedAt = time.Now()
	}
	state := m.state
	state.Chapters = append([]player.Chapter(nil), m.state.Chapters...)
	m.stateMu.Unlock()
	return state
}

func (m *MPV) waitLoop(processDone <-chan error, output *boundedOutput) {
	err := <-processDone
	m.emit(player.Event{Kind: player.EventClosed, Err: withPlayerOutput(err, output)})
	m.Close()
}

func withPlayerOutput(err error, output *boundedOutput) error {
	if err == nil {
		err = errors.New("player exited")
	}
	detail := output.String()
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func (m *MPV) emit(event player.Event) { m.events.Push(event) }

func (m *MPV) Close() error {
	var result error
	m.once.Do(func() {
		m.requestQuit()
		close(m.done)
		m.completeLoad(errors.New("player closed while loading media"))
		result = m.stream.Close()
		m.killTree()
		m.cleanup()
		m.events.Close()
	})
	return result
}

// requestQuit asks the player to quit over IPC before its process is
// signalled. For IINA the process Faro started is only the iina-cli wrapper,
// so a signal alone would leave IINA's window open; the quit command reaches
// IINA's own mpv core. The reply is not awaited.
func (m *MPV) requestQuit() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = m.send(ctx, []byte(`{"command":["quit"]}`+"\n"), nil)
}

func (m *MPV) Done() <-chan struct{} { return m.done }
