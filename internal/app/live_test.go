package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// liveResolver resolves every link to a live broadcast and serves a chat.
type liveResolver struct {
	chats atomic.Int32
}

func (r *liveResolver) Inspect(context.Context, string) (youtube.Info, error) {
	return youtube.Info{Title: "Live show", Live: true}, nil
}

func (r *liveResolver) Resolve(context.Context, string, int) (youtube.Stream, error) {
	return youtube.Stream{CombinedURL: "https://cdn.example.test/live.m3u8", Info: youtube.Info{Title: "Live show", Live: true}}, nil
}

func (r *liveResolver) LiveChat(ctx context.Context, _ string, deliver func(youtube.ChatUpdate)) error {
	r.chats.Add(1)
	deliver(youtube.ChatUpdate{Messages: []youtube.ChatMessage{{ID: "m1", Author: "@viewer", Text: "hello"}}})
	<-ctx.Done()
	return ctx.Err()
}

// recordingSink collects events for assertions.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) record(event Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recordingSink) matching(match func(Event) bool) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Event
	for _, event := range r.events {
		if match(event) {
			result = append(result, event)
		}
	}
	return result
}

// liveRoom hosts a room for s and returns the players it starts.
func liveRoom(t *testing.T, sink EventSink) (*Service, func() *lifecyclePlayer) {
	t.Helper()
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	s := New(context.Background(), sink)
	s.autoOffer = false
	var mu sync.Mutex
	var players []*lifecyclePlayer
	s.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) {
		mediaPlayer := newLifecyclePlayer()
		mu.Lock()
		players = append(players, mediaPlayer)
		mu.Unlock()
		return mediaPlayer, nil
	}
	status, err := s.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", Room: "room"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	if err := s.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	return s, func() *lifecyclePlayer {
		mu.Lock()
		defer mu.Unlock()
		if len(players) == 0 {
			return nil
		}
		return players[len(players)-1]
	}
}

// observe joins the room as another participant and counts the media
// updates it receives.
func observe(t *testing.T, s *Service) *atomic.Int32 {
	t.Helper()
	parsed, err := invite.Parse(s.ServerStatus().LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	observer, err := faroclient.Connect(ctx, faroclient.Config{
		Address: parsed.Address, Fingerprint: parsed.Fingerprint, JoinToken: parsed.JoinToken,
		Name: "observer", Room: parsed.Room, ClientVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close() })
	var updates atomic.Int32
	go func() {
		for {
			select {
			case event := <-observer.Events():
				if event.Type == protocol.TypeMediaUpdated {
					updates.Add(1)
				}
			case <-observer.Done():
				return
			}
		}
	}()
	return &updates
}

func playURL(t *testing.T, s *Service, url string, current func() *lifecyclePlayer) *lifecyclePlayer {
	t.Helper()
	if err := s.SetPlaylist([]PlaylistInput{{URL: url, Label: "stream"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stream never opened", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.selectedItem != "" && s.player != nil && !s.openingMedia
	})
	return current()
}

// A live stream's duration changes many times a second. Faro announces the
// media once, then once more when it recognizes the stream as live, instead
// of turning every change into a room command.
func TestGrowingLiveDurationDoesNotFloodTheRoom(t *testing.T) {
	previous := liveProbeWindow
	liveProbeWindow = 500 * time.Millisecond
	t.Cleanup(func() { liveProbeWindow = previous })
	sink := &recordingSink{}
	s, current := liveRoom(t, sink.record)
	updates := observe(t, s)
	mediaPlayer := playURL(t, s, "https://example.test/live.m3u8", current)
	time.Sleep(100 * time.Millisecond)
	before := updates.Load()
	began := time.Now()
	for time.Since(began) < 1500*time.Millisecond {
		mediaPlayer.mu.Lock()
		mediaPlayer.state.DurationSeconds = 6 + time.Since(began).Seconds()
		state := mediaPlayer.state
		mediaPlayer.mu.Unlock()
		mediaPlayer.events <- player.Event{Kind: player.EventMedia, State: state}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, "the growing stream was not recognized as live", func() bool {
		snapshot, _ := s.Snapshot()
		for _, participant := range snapshot.Participants {
			if participant.ID == snapshot.SelfID && participant.Media != nil {
				return participant.Media.Live
			}
		}
		return false
	})
	if errors := sink.matching(func(event Event) bool { return event.Error != nil && event.Error.Code == "media_publish" }); len(errors) != 0 {
		t.Fatalf("publishing media failed: %v", errors[0].Error.Message)
	}
	if sent := updates.Load() - before; sent > 4 {
		t.Fatalf("about 150 duration changes became %d room media updates", sent)
	}
}

// A YouTube broadcast that is live is opened at the player's own position:
// only playing and pausing follow the room, room seeks are refused, and its
// chat is followed until something else plays.
func TestLiveBroadcastSharesOnlyPlayingAndPausing(t *testing.T) {
	sink := &recordingSink{}
	s, current := liveRoom(t, sink.record)
	resolver := &liveResolver{}
	s.mu.Lock()
	s.youtube = resolver
	s.mu.Unlock()
	client, _ := s.connected()
	if err := client.SetPlayback(protocol.PlaybackSet{PositionSeconds: 400, Paused: true, Rate: 1}); err != nil {
		t.Fatal(err)
	}
	mediaPlayer := playURL(t, s, "https://www.youtube.com/watch?v=livelivelive", current)
	if state, _ := mediaPlayer.State(context.Background()); state.PositionSeconds == 400 {
		t.Fatal("the live broadcast was seeked to the room's position")
	}
	if err := s.Seek(30); !errors.Is(err, errLiveControl) {
		t.Fatalf("a room seek in a live broadcast returned %v", err)
	}
	if err := s.SetRate(2); !errors.Is(err, errLiveControl) {
		t.Fatalf("a speed change in a live broadcast returned %v", err)
	}
	waitFor(t, "the broadcast's chat did not reach the page", func() bool {
		return len(sink.matching(func(event Event) bool {
			return event.StreamChat != nil && len(event.StreamChat.Messages) == 1 && event.StreamChat.Messages[0].Text == "hello"
		})) > 0
	})
	if err := s.SetPlaylist([]PlaylistInput{{URL: "https://example.test/video.mp4", Label: "video"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the chat outlived its broadcast", func() bool {
		return len(sink.matching(func(event Event) bool { return event.StreamChat != nil && event.StreamChat.State == "ended" })) > 0
	})
	s.mu.RLock()
	live, chat := s.currentLive, s.liveChat
	s.mu.RUnlock()
	if live || chat != nil || resolver.chats.Load() != 1 {
		t.Fatalf("live state survived the next item (live %v, chat %v, %d chats)", live, chat != nil, resolver.chats.Load())
	}
}

// A network source that fails after loading is reloaded, a bounded number
// of times.
func TestFailedNetworkSourceIsReloaded(t *testing.T) {
	previous := playbackRecoveryDelay
	playbackRecoveryDelay = 20 * time.Millisecond
	t.Cleanup(func() { playbackRecoveryDelay = previous })
	s, current := liveRoom(t, nil)
	const url = "https://example.test/live.m3u8"
	mediaPlayer := playURL(t, s, url, current)
	for attempt := 1; attempt <= maxPlaybackRecoveries+1; attempt++ {
		mediaPlayer.mu.Lock()
		state := mediaPlayer.state
		mediaPlayer.state.Source = ""
		mediaPlayer.mu.Unlock()
		mediaPlayer.events <- player.Event{Kind: player.EventFailed, State: state, Err: errors.New("playback failed: connection lost")}
		if attempt > maxPlaybackRecoveries {
			break
		}
		waitFor(t, "the failed source was not reloaded", func() bool {
			state, _ := mediaPlayer.State(context.Background())
			return state.Source == url
		})
	}
	time.Sleep(300 * time.Millisecond)
	if state, _ := mediaPlayer.State(context.Background()); state.Source == url {
		t.Fatal("a source that keeps failing was reloaded without limit")
	}
}
