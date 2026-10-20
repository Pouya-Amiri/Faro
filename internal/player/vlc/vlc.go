package vlc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/player/discovery"
)

type Config struct {
	Executable string
	ExtraArgs  []string
}

// commandTimeout bounds one remote-control exchange.
const commandTimeout = 3 * time.Second

// maxOwedReplies is how many abandoned commands may still owe a reply
// before the remote-control session counts as unresponsive.
const maxOwedReplies = 3

type VLC struct {
	process *exec.Cmd
	conn    net.Conn
	reader  *bufio.Reader
	rate    bool
	// turn admits one command at a time; it is a channel so that waiting
	// for it honours cancellation. owed counts replies of commands that
	// were abandoned after being sent, which the next command reads first
	// so that it never takes another command's reply for its own. Both
	// owed and the reader belong to whoever holds turn.
	turn    chan struct{}
	owed    int
	stateMu sync.RWMutex
	state   player.State
	// opened is the MRL Faro last opened and source what it was opened
	// from, to tell Faro's own media from a file chosen in VLC itself.
	opened string
	source string
	events *player.EventQueue
	done   chan struct{}
	once   sync.Once
}

func Start(ctx context.Context, cfg Config, initialSource string) (*VLC, error) {
	executable := cfg.Executable
	if executable == "" {
		var findErr error
		executable, findErr = discovery.Find("vlc")
		if findErr != nil {
			return nil, findErr
		}
	}
	address, err := reserveAddress()
	if err != nil {
		return nil, err
	}
	args := launchArguments(runtime.GOOS, address, cfg.ExtraArgs, initialSource)
	command := exec.CommandContext(ctx, discovery.Resolve(executable), args...)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start VLC: %w", err)
	}
	connection, err := waitForRC(ctx, address)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	initialState := player.State{Paused: true, Rate: 1, ObservedAt: time.Now()}
	if initialSource != "" {
		initialState.Source = initialSource
		initialState.Title = filepath.Base(initialSource)
	}
	instance := newVLC(connection)
	instance.process, instance.state = command, initialState
	if initialSource != "" {
		instance.opened, _ = vlcMRL(initialSource)
		instance.source = initialSource
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = instance.reader.ReadString('>')
	_ = connection.SetReadDeadline(time.Time{})
	if response, helpErr := instance.command(ctx, "help"); helpErr == nil {
		instance.rate = hasRCCommand(response, "rate")
	}
	go instance.pollLoop()
	go instance.waitLoop()
	return instance, nil
}

func newVLC(connection net.Conn) *VLC {
	return &VLC{
		conn: connection, reader: bufio.NewReaderSize(connection, 32*1024), turn: make(chan struct{}, 1),
		state: player.State{Paused: true, Rate: 1, ObservedAt: time.Now()}, events: player.NewEventQueue(), done: make(chan struct{}),
	}
}

func launchArguments(operatingSystem string, address string, extra []string, initialSource string) []string {
	args := make([]string, 0, len(extra)+7)
	args = append(args, safeExtraArguments(extra)...)
	// Target the Lua "cli" interface explicitly. The plain "rc" shortcut is
	// registered by two different implementations: the C oldrc module on
	// Windows and the Lua cli elsewhere. oldrc never prints the "> " prompt
	// this adapter reads and reports state with legacy wording, so commands
	// could never complete against it. The Lua cli exists on every platform
	// VLC ships and provides the prompt plus the "( state ... )" responses.
	if operatingSystem == "darwin" {
		// VLC's macOS interface must stay the main interface.
		args = append(args, "--extraintf=luaintf")
	} else {
		// VLC starts its extra interfaces before the main one, and waits for
		// the Qt interface to be ready only when it is an extra interface.
		// With the Lua cli as the main interface, the control socket answers
		// once Qt can host the video. Otherwise media Faro opens straight
		// after starting VLC gets a separate video window, because the Qt
		// window does not exist yet.
		args = append(args, "--intf=luaintf", "--extraintf=qt")
	}
	args = append(args, "--lua-intf=cli", "--cli-host="+address, "--no-video-title-show")
	if operatingSystem != "darwin" {
		// Faro needs a dedicated process so its control socket and lifecycle cannot
		// be redirected to an unrelated VLC window by the user's one-instance
		// setting. VLC's macOS interface does not expose these Qt/Windows options;
		// launching the app-bundle executable already creates the owned process.
		args = append(args, "--no-one-instance", "--no-one-instance-when-started-from-file")
	}
	if initialSource != "" {
		args = append(args, initialSource)
	}
	return args
}

func safeExtraArguments(values []string) []string {
	result := make([]string, 0, len(values))
	for index := 0; index < len(values); index++ {
		value := values[index]
		key := strings.ToLower(strings.SplitN(value, "=", 2)[0])
		consumesValue := false
		if len(value) > 2 && strings.EqualFold(value[:2], "-I") && value[1] != '-' {
			continue
		}
		switch key {
		case "-i", "--intf", "--extraintf", "--lua-intf", "--cli-host", "--rc-host":
			consumesValue = strings.EqualFold(value, key)
		case "--no-extraintf", "--rc-quiet", "--no-rc-quiet",
			"--one-instance", "--no-one-instance",
			"--one-instance-when-started-from-file", "--no-one-instance-when-started-from-file":
		default:
			result = append(result, value)
			continue
		}
		if consumesValue && index+1 < len(values) {
			index++
		}
	}
	return result
}

func reserveAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

func waitForRC(ctx context.Context, address string) (net.Conn, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			return connection, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, errors.New("timed out waiting for VLC remote control")
		case <-ticker.C:
		}
	}
}

func (v *VLC) Capabilities() player.Capabilities {
	return player.Capabilities{PositionResolution: time.Second, PlaybackRate: v.rate, URLs: true}
}

// State polls VLC. The remote-control interface reports the playback state,
// the current input, position and length; it cannot report the playback
// rate or buffering, so Rate is the last rate Faro set and Buffering is
// always false.
func (v *VLC) State(ctx context.Context) (player.State, error) {
	status, err := v.command(ctx, "status")
	if err != nil {
		return player.State{}, err
	}
	playback, err := parsePlaybackState(status)
	if err != nil {
		return player.State{}, err
	}
	input := parseInput(status)
	duration, position := float64(0), float64(0)
	if input != "" {
		duration, _ = v.number(ctx, "get_length")
		if playback == playbackStopped {
			// VLC often returns an empty get_time response once stopped.
			position = duration
		} else {
			// VLC 3's documented RC surface only exposes whole-second get_time.
			position, err = v.number(ctx, "get_time")
			if err != nil {
				return player.State{}, err
			}
		}
	}
	v.stateMu.Lock()
	switch {
	case input == "":
		// Nothing is loaded: the file ended or was stopped.
		v.state.Source, v.state.Title = "", ""
	case input == v.opened:
		v.state.Source = v.source
	default:
		// Someone opened another file in VLC itself.
		v.opened, v.source = input, sourceOfMRL(input)
		v.state.Source, v.state.Title = v.source, filepath.Base(v.source)
	}
	v.state.PositionSeconds = position
	v.state.DurationSeconds = duration
	v.state.Paused = playback != playbackPlaying
	v.state.ObservedAt = time.Now()
	state := v.state
	v.stateMu.Unlock()
	return state, nil
}

// parseInput returns the MRL of VLC's current input from a status reply, or
// "" when nothing is loaded.
func parseInput(response string) string {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "( new input: "); ok {
			return strings.TrimSpace(strings.TrimSuffix(value, ")"))
		}
	}
	return ""
}

// sourceOfMRL turns a file MRL back into a local path; other MRLs are kept.
func sourceOfMRL(mrl string) string {
	parsed, err := url.Parse(mrl)
	if err != nil || parsed.Scheme != "file" {
		return mrl
	}
	path := parsed.Path
	if parsed.Host != "" {
		path = "//" + parsed.Host + path
	}
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, "/")
	}
	return filepath.FromSlash(path)
}

func (v *VLC) SetPaused(ctx context.Context, paused bool) error {
	state, err := v.State(ctx)
	if err != nil {
		return err
	}
	if state.Paused == paused {
		return nil
	}
	_, err = v.command(ctx, "pause")
	return err
}

func (v *VLC) Seek(ctx context.Context, seconds float64) error {
	target, err := seekArgument(seconds)
	if err != nil {
		return err
	}
	_, err = v.command(ctx, "seek "+target)
	return err
}

func seekArgument(seconds float64) (string, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "", errors.New("VLC seek target must be finite")
	}
	if seconds < 0 {
		seconds = 0
	}
	// VLC 3 RC accepts whole seconds. A decimal such as "1.250" is parsed as
	// 250 seconds, which can immediately push short media to end-of-file.
	return strconv.FormatFloat(seconds, 'f', 0, 64), nil
}

func (v *VLC) SetRate(ctx context.Context, rate float64) error {
	if !v.rate {
		return errors.New("this VLC version does not expose playback-rate control through RC")
	}
	_, err := v.command(ctx, "rate "+strconv.FormatFloat(rate, 'f', 4, 64))
	if err == nil {
		v.stateMu.Lock()
		v.state.Rate = rate
		v.stateMu.Unlock()
	}
	return err
}

func (v *VLC) Open(ctx context.Context, source string) error {
	mrl, err := vlcMRL(source)
	if err != nil {
		return err
	}
	// VLC's CLI interface does not strip shell-style quotes: `add "file:///x"`
	// attempts to open an MRL whose first and last characters are literal quotes.
	// A percent-encoded MRL is safe to send as the command's unquoted argument.
	if _, err := v.command(ctx, "stop"); err != nil {
		return fmt.Errorf("stop current VLC input: %w", err)
	}
	if _, err := v.command(ctx, "clear"); err != nil {
		return fmt.Errorf("clear VLC playlist: %w", err)
	}
	v.stateMu.Lock()
	v.state.Source = ""
	v.state.Title = ""
	v.state.PositionSeconds = 0
	v.state.DurationSeconds = 0
	v.state.Paused = true
	v.state.ObservedAt = time.Now()
	v.stateMu.Unlock()
	if err := validateLocalSource(source); err != nil {
		return err
	}
	v.stateMu.Lock()
	v.opened, v.source = mrl, source
	v.stateMu.Unlock()
	if _, err := v.command(ctx, "add "+mrl); err != nil {
		return err
	}
	if err := v.waitForInput(ctx); err != nil {
		return fmt.Errorf("VLC did not load %q: %w", source, err)
	}
	v.stateMu.Lock()
	v.state.Source = source
	v.state.Title = filepath.Base(source)
	v.state.ObservedAt = time.Now()
	state := v.state
	v.stateMu.Unlock()
	v.emit(player.Event{Kind: player.EventMedia, State: state})
	return nil
}

func validateLocalSource(source string) error {
	if !filepath.IsAbs(source) {
		parsed, err := url.Parse(source)
		if err == nil && parsed.Scheme != "" {
			return nil
		}
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("open local media %q: %w", source, err)
	}
	if info.IsDir() {
		return fmt.Errorf("open local media %q: path is a directory", source)
	}
	return nil
}

func (v *VLC) waitForInput(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		response, err := v.command(ctx, "status")
		if err != nil {
			return err
		}
		state, parseErr := parsePlaybackState(response)
		if parseErr == nil && state != playbackStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("timed out waiting for an active input")
		case <-ticker.C:
		}
	}
}

func vlcMRL(source string) (string, error) {
	if strings.ContainsAny(source, "\r\n") {
		return "", errors.New("media source contains a line break")
	}
	if !filepath.IsAbs(source) {
		parsed, err := url.Parse(source)
		if err == nil && parsed.Scheme != "" {
			return parsed.String(), nil
		}
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve media path: %w", err)
	}
	return localVLCMRL(absolute, runtime.GOOS), nil
}

func localVLCMRL(absolute, operatingSystem string) string {
	slashed := filepath.ToSlash(absolute)
	if operatingSystem == "windows" {
		slashed = strings.ReplaceAll(absolute, `\`, "/")
		if strings.HasPrefix(slashed, "//") {
			unc := strings.TrimPrefix(slashed, "//")
			host, path, _ := strings.Cut(unc, "/")
			return (&url.URL{Scheme: "file", Host: host, Path: "/" + path}).String()
		}
	}
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

func (v *VLC) Events() <-chan player.Event { return v.events.Events() }

func (v *VLC) number(ctx context.Context, command string) (float64, error) {
	response, err := v.command(ctx, command)
	if err != nil {
		return 0, err
	}
	value, err := parseNumberResponse(response)
	if err != nil {
		return 0, fmt.Errorf("VLC returned no number for %s", command)
	}
	return value, nil
}

func parseNumberResponse(response string) (float64, error) {
	// A command result is a line by itself. Parsing arbitrary numeric tokens can
	// mistake unsolicited status output (for example volume) for the response.
	for _, line := range strings.Split(response, "\n") {
		if value, parseErr := strconv.ParseFloat(strings.TrimSpace(line), 64); parseErr == nil {
			return value, nil
		}
	}
	return 0, errors.New("response did not contain a numeric result line")
}

// command sends one remote-control command and returns its reply. Waiting
// for the channel, writing and reading are all bounded by ctx and
// commandTimeout, and cancelling ctx interrupts I/O in progress, so a
// retired operation never issues a stale command. A reply that was not
// waited for is skipped by the next command; a session that keeps failing
// to reply, or whose input was cut mid-command, retires the player.
func (v *VLC) command(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	deadline := time.Now().Add(commandTimeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	wait := time.NewTimer(time.Until(deadline))
	defer wait.Stop()
	select {
	case v.turn <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-wait.C:
		return "", errors.New("VLC is not accepting commands")
	case <-v.done:
		return "", errors.New("VLC is closed")
	}
	defer func() { <-v.turn }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_ = v.conn.SetDeadline(deadline)
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = v.conn.SetDeadline(time.Now())
	})
	defer func() {
		// An interruption already under way must finish first, or it would
		// cut short the next command.
		if !stopInterrupt() {
			<-interrupted
		}
		_ = v.conn.SetDeadline(time.Time{})
	}()
	for v.owed > 0 {
		if _, err := v.reader.ReadString('>'); err != nil {
			return "", v.unanswered(ctx, err)
		}
		v.owed--
	}
	line := command + "\n"
	if written, err := io.WriteString(v.conn, line); err != nil {
		if written > 0 {
			// VLC received part of a command; nothing it says can be
			// matched to a command any more.
			go v.Close()
		}
		return "", err
	}
	response, err := v.reader.ReadString('>')
	if err != nil {
		v.owed++
		return "", v.unanswered(ctx, err)
	}
	response = strings.TrimSpace(strings.TrimSuffix(response, ">"))
	if err := responseError(command, response); err != nil {
		return response, err
	}
	return response, nil
}

// unanswered handles a reply that did not arrive in time. The caller holds
// turn.
func (v *VLC) unanswered(ctx context.Context, err error) error {
	if v.owed > maxOwedReplies {
		go v.Close()
		return fmt.Errorf("VLC stopped answering commands: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

type playbackState uint8

const (
	playbackStopped playbackState = iota
	playbackPaused
	playbackPlaying
)

func parsePlaybackState(response string) (playbackState, error) {
	state := playbackStopped
	found := false
	for _, line := range strings.Split(strings.ToLower(response), "\n") {
		switch {
		case strings.Contains(line, "( state playing )"):
			state, found = playbackPlaying, true
		case strings.Contains(line, "( state paused )"):
			state, found = playbackPaused, true
		case strings.Contains(line, "( state stopped )"):
			state, found = playbackStopped, true
		}
	}
	if !found {
		return playbackStopped, fmt.Errorf("VLC status did not contain a playback state: %q", response)
	}
	return state, nil
}

func responseError(command, response string) error {
	for _, line := range strings.Split(response, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "unknown command") || strings.HasPrefix(lower, "error in ") {
			return fmt.Errorf("VLC command %q failed: %s", command, trimmed)
		}
		fields := strings.Fields(lower)
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] != "returned" {
				continue
			}
			code, err := strconv.Atoi(strings.Trim(fields[index+1], "(),:;"))
			if err == nil && code < 0 {
				return fmt.Errorf("VLC command %q failed: %s", command, trimmed)
			}
		}
	}
	return nil
}

func hasRCCommand(response, command string) bool {
	for _, line := range strings.Split(response, "\n") {
		fields := strings.Fields(strings.TrimLeft(strings.TrimSpace(line), "|+- "))
		if len(fields) > 0 && fields[0] == command {
			return true
		}
	}
	return false
}

func (v *VLC) pollLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var previous player.State
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			state, err := v.State(ctx)
			cancel()
			if err != nil {
				continue
			}
			switch {
			case state.Source != previous.Source || state.DurationSeconds != previous.DurationSeconds:
				v.emit(player.Event{Kind: player.EventMedia, State: state})
			case state.PositionSeconds != previous.PositionSeconds || state.Paused != previous.Paused:
				v.emit(player.Event{Kind: player.EventState, State: state})
			}
			previous = state
		case <-v.done:
			return
		}
	}
}

func (v *VLC) waitLoop() {
	err := v.process.Wait()
	v.emit(player.Event{Kind: player.EventClosed, Err: err})
	v.Close()
}

func (v *VLC) emit(event player.Event) { v.events.Push(event) }

// Close asks VLC to quit when the control channel is free, without ever
// waiting on it, then closes the channel (ending any command in progress)
// and terminates the process.
func (v *VLC) Close() error {
	var result error
	v.once.Do(func() {
		close(v.done)
		select {
		case v.turn <- struct{}{}:
			_ = v.conn.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
			_, _ = io.WriteString(v.conn, "quit\n")
			<-v.turn
		default:
		}
		result = v.conn.Close()
		if v.process != nil && v.process.Process != nil {
			_ = v.process.Process.Kill()
		}
		v.events.Close()
	})
	return result
}

func (v *VLC) Done() <-chan struct{} { return v.done }
