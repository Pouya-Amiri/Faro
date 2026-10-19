package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
)

func TestEndFileErrorCompletesPendingLoadWithReason(t *testing.T) {
	loaded := make(chan error, 1)
	instance := &MPV{loaded: loaded}
	instance.handleEvent(response{Event: "end-file", Reason: "error", FileError: "loading failed"})
	if err := <-loaded; err == nil || !strings.Contains(err.Error(), "loading failed") {
		t.Fatalf("end-file error produced %v", err)
	}
}

func TestWaitForIPCReturnsEarlyPlayerExitDiagnostics(t *testing.T) {
	processDone := make(chan error, 1)
	processDone <- errors.New("startup failed")
	output := &boundedOutput{}
	_, _ = output.Write([]byte("mpv.net diagnostic"))
	_, err := waitForIPC(context.Background(), processDone, filepath.Join(t.TempDir(), "missing-ipc"), output)
	if err == nil || !strings.Contains(err.Error(), "startup failed") || !strings.Contains(err.Error(), "mpv.net diagnostic") {
		t.Fatalf("waitForIPC error = %v", err)
	}
}

func TestIsolateMPVNetReplacesUserConfigEnvironmentAndCleansUp(t *testing.T) {
	command := exec.Command("mpvnet")
	t.Setenv("MPVNET_HOME", filepath.Join(t.TempDir(), "user-config"))
	baseCleaned := false
	cleanup, err := isolateMPVNet(command, func() { baseCleaned = true })
	if err != nil {
		t.Fatal(err)
	}
	var isolated string
	for _, entry := range command.Env {
		if strings.HasPrefix(strings.ToUpper(entry), "MPVNET_HOME=") {
			if isolated != "" {
				t.Fatalf("duplicate MPVNET_HOME entries in %v", command.Env)
			}
			isolated = strings.SplitN(entry, "=", 2)[1]
		}
	}
	if info, statErr := os.Stat(isolated); isolated == "" || statErr != nil || !info.IsDir() {
		t.Fatalf("isolated profile was not created: %q, %v", isolated, statErr)
	}
	cleanup()
	if !baseCleaned {
		t.Fatal("base player cleanup was not called")
	}
	if _, statErr := os.Stat(isolated); !os.IsNotExist(statErr) {
		t.Fatalf("isolated profile was not removed: %v", statErr)
	}
}

func TestStateRefreshDoesNotEmitSyntheticPlayerEvent(t *testing.T) {
	instance := &MPV{events: player.NewEventQueue()}
	instance.updateProperty("path", json.RawMessage(`"movie.mkv"`))
	select {
	case event := <-instance.Events():
		t.Fatalf("state refresh emitted a synthetic event: %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
	if instance.state.Source != "movie.mkv" {
		t.Fatalf("state was not updated: %#v", instance.state)
	}
}

func TestVerifyLoadedSourceRejectsAStaleLocalFile(t *testing.T) {
	requested := filepath.Join(t.TempDir(), "requested.mkv")
	loaded := filepath.Join(t.TempDir(), "stale.mkv")
	if err := os.WriteFile(requested, []byte("requested"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loaded, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyLoadedSource(requested, requested); err != nil {
		t.Fatalf("matching media was rejected: %v", err)
	}
	if err := verifyLoadedSource(requested, loaded); err == nil {
		t.Fatal("stale loaded media was accepted")
	}
}

func TestVerifyLoadedSourceAllowsRedirectedNetworkURL(t *testing.T) {
	if err := verifyLoadedSource("https://media.example/watch", "https://cdn.example/video.mkv"); err != nil {
		t.Fatalf("redirected network media was rejected: %v", err)
	}
}

func TestSeekUsesMPVAbsoluteExactCommand(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	instance := newMPV(client, wedgedWriteTimeout)
	t.Cleanup(func() { instance.Close() })
	commandSeen := make(chan []any, 1)
	go func() {
		line, _ := bufio.NewReader(server).ReadBytes('\n')
		var request struct {
			Command   []any `json:"command"`
			RequestID int64 `json:"request_id"`
		}
		_ = json.Unmarshal(line, &request)
		commandSeen <- request.Command
		instance.pendingMu.Lock()
		pending := instance.pending[request.RequestID]
		instance.pendingMu.Unlock()
		pending <- response{RequestID: request.RequestID, Error: "success"}
	}()
	if err := instance.Seek(context.Background(), 37.25); err != nil {
		t.Fatal(err)
	}
	command := <-commandSeen
	if len(command) != 3 || command[0] != "seek" || command[1] != 37.25 || command[2] != "absolute+exact" {
		t.Fatalf("unexpected seek command: %#v", command)
	}
}

func TestObservedPropertyStillEmitsPlayerEvent(t *testing.T) {
	instance := &MPV{events: player.NewEventQueue(), done: make(chan struct{})}
	instance.applyProperty("path", json.RawMessage(`"movie.mkv"`))
	select {
	case event := <-instance.Events():
		if event.Kind != player.EventMedia || event.State.Source != "movie.mkv" {
			t.Fatalf("unexpected observed event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("observed property did not emit an event")
	}
}

func TestChapterListIsExposedInPlayerState(t *testing.T) {
	instance := &MPV{}
	instance.updateProperty("chapter-list", json.RawMessage(`[{"title":"Opening","time":0},{"title":"Act two","time":42.5}]`))
	if len(instance.state.Chapters) != 2 || instance.state.Chapters[1].Title != "Act two" || instance.state.Chapters[1].StartSeconds != 42.5 {
		t.Fatalf("unexpected chapters: %#v", instance.state.Chapters)
	}
}

func TestStateRejectsClosedPlayer(t *testing.T) {
	done := make(chan struct{})
	close(done)
	m := &MPV{done: done}
	if _, err := m.State(context.Background()); err == nil {
		t.Fatal("closed player reported a healthy cached state")
	}
}

func TestCommandGivesUpOnAControlChannelThatStoppedReading(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	instance := newMPV(client, time.Minute)
	t.Cleanup(func() { instance.Close() })
	// Nothing reads the other end, so the first write never completes.
	go func() { _ = instance.SetPaused(context.Background(), true) }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan error, 1)
	go func() { finished <- instance.Seek(ctx, 1) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("a cancelled command reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled command waited for a wedged control channel")
	}
	finished = make(chan error, 1)
	go func() { finished <- instance.Seek(context.Background(), 1) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("a command on a wedged control channel reported success")
		}
	case <-time.After(requestTimeout + time.Second):
		t.Fatal("a command on a wedged control channel outlived its timeout")
	}
}

func TestWedgedControlChannelRetiresThePlayer(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	instance := newMPV(client, 100*time.Millisecond)
	go func() { _ = instance.SetPaused(context.Background(), true) }()
	select {
	case <-instance.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a player whose control channel stopped reading was not retired")
	}
}

func TestUnavailablePropertiesClearTheOldMedia(t *testing.T) {
	instance := &MPV{}
	instance.updateProperty("path", json.RawMessage(`"old.mkv"`))
	instance.updateProperty("duration", json.RawMessage(`120`))
	instance.updateProperty("media-title", json.RawMessage(`"Old"`))
	instance.updateProperty("path", json.RawMessage(nil))
	instance.updateProperty("media-title", json.RawMessage(`null`))
	state := instance.updateProperty("duration", json.RawMessage(nil))
	if state.Source != "" || state.DurationSeconds != 0 || state.Title != "" {
		t.Fatalf("unavailable properties kept the old media: %#v", state)
	}
}

func TestIdlePlayerReportsNothingLoaded(t *testing.T) {
	instance := &MPV{events: player.NewEventQueue(), done: make(chan struct{})}
	instance.updateProperty("path", json.RawMessage(`"movie.mkv"`))
	instance.updateProperty("duration", json.RawMessage(`120`))
	instance.updateProperty("time-pos", json.RawMessage(`119.5`))
	instance.applyProperty("idle-active", json.RawMessage(`true`))
	select {
	case event := <-instance.Events():
		if event.Kind != player.EventMedia || event.State.Source != "" || event.State.DurationSeconds != 0 || event.State.PositionSeconds != 0 {
			t.Fatalf("an idle player still reports media: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("going idle was not reported")
	}
}

func TestPlaybackFailureAfterLoadingIsReported(t *testing.T) {
	instance := &MPV{events: player.NewEventQueue(), done: make(chan struct{})}
	instance.handleEvent(response{Event: "end-file", Reason: "error", FileError: "network connection lost"})
	select {
	case event := <-instance.Events():
		if event.Kind != player.EventFailed || event.Err == nil || !strings.Contains(event.Err.Error(), "network connection lost") {
			t.Fatalf("unexpected failure event: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("a playback failure after loading was not reported")
	}
}
