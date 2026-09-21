package app

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/syncer"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

func TestValidatePlayerRejectsProfilesFromOtherPlatforms(t *testing.T) {
	if err := validatePlayer("mpv"); err != nil {
		t.Fatalf("mpv should be supported on %s: %v", runtime.GOOS, err)
	}
	unsupported := map[string]string{"darwin": "mpv.net", "windows": "iina", "linux": "iina"}[runtime.GOOS]
	if unsupported != "" && validatePlayer(unsupported) == nil {
		t.Fatalf("%s should not be supported on %s", unsupported, runtime.GOOS)
	}
}

type sponsorClient struct {
	snapshot protocol.Snapshot
	updates  []protocol.PlaybackSet
}

func (c *sponsorClient) Snapshot() protocol.Snapshot { return c.snapshot }
func (c *sponsorClient) SetPlayback(value protocol.PlaybackSet) error {
	c.updates = append(c.updates, value)
	return nil
}

func TestObserveLocalChangeDetectsPlayerSeek(t *testing.T) {
	now := time.Now()
	service := New(context.Background(), nil)
	service.lastPlayer = player.State{PositionSeconds: 10, Paused: true, Rate: 1, ObservedAt: now}

	changed, seek := service.observeLocalChange(player.State{PositionSeconds: 30, Paused: true, Rate: 1, ObservedAt: now.Add(time.Millisecond)})
	if !changed || !seek {
		t.Fatalf("player seek was not detected: changed=%v seek=%v", changed, seek)
	}
}

func TestObserveLocalChangeSuppressesProgrammaticPlayback(t *testing.T) {
	now := time.Now()
	service := New(context.Background(), nil)
	service.lastPlayer = player.State{PositionSeconds: 10, Paused: true, Rate: 1, ObservedAt: now}
	service.suppressUntil = time.Now().Add(time.Second)

	changed, seek := service.observeLocalChange(player.State{PositionSeconds: 30, Paused: false, Rate: 1.25, ObservedAt: now.Add(time.Millisecond)})
	if changed || seek {
		t.Fatalf("programmatic playback event should be suppressed: changed=%v seek=%v", changed, seek)
	}
}

func TestSetRateRejectsOutOfRangeValueBeforeConnecting(t *testing.T) {
	service := New(context.Background(), nil)
	for _, rate := range []float64{0, 0.24, 4.01} {
		if err := service.SetRate(rate); err == nil {
			t.Fatalf("SetRate(%v) should fail", rate)
		}
	}
}

func TestShouldApplyRemoteAcceptsSelfAuthoredPlayback(t *testing.T) {
	service := New(context.Background(), nil)
	service.sync = syncer.New(nil, nil, nil)
	snapshot := protocol.Snapshot{
		SelfID: "self", Playback: protocol.Playback{SetBy: "self"},
		Participants: []protocol.Participant{{ID: "self", Media: &protocol.Media{Fingerprint: "same"}}},
	}
	if !service.shouldApplyRemote(snapshot) {
		t.Fatal("self-authored playback must recover drift after local intent has settled")
	}
}

func TestShouldApplyRemoteAcceptsMatchingForeignController(t *testing.T) {
	service := New(context.Background(), nil)
	service.sync = syncer.New(nil, nil, nil)
	snapshot := protocol.Snapshot{
		SelfID: "self", Playback: protocol.Playback{SetBy: "other"},
		Participants: []protocol.Participant{
			{ID: "self", Media: &protocol.Media{Fingerprint: "same"}},
			{ID: "other", Media: &protocol.Media{Fingerprint: "same"}},
		},
	}
	if !service.shouldApplyRemote(snapshot) {
		t.Fatal("matching playback from another controller was ignored")
	}
}

func TestShouldApplyRemoteWaitsOnlyForPendingLocalIntent(t *testing.T) {
	service := New(context.Background(), nil)
	service.sync = syncer.New(nil, nil, nil)
	service.localPlaybackPending = true
	snapshot := protocol.Snapshot{
		SelfID: "self", Playback: protocol.Playback{SetBy: "other"},
		Participants: []protocol.Participant{
			{ID: "self", Media: &protocol.Media{Fingerprint: "same"}},
			{ID: "other", Media: &protocol.Media{Fingerprint: "same"}},
		},
	}
	if service.shouldApplyRemote(snapshot) {
		t.Fatal("remote playback raced an unpublished local player command")
	}
	service.localPlaybackPending = false
	if !service.shouldApplyRemote(snapshot) {
		t.Fatal("remote playback remained blocked after local publication")
	}
}

func TestSponsorBlockSkipsOnceAndPublishesRoomSeek(t *testing.T) {
	service := New(context.Background(), nil)
	service.sponsorSegments = []youtube.Segment{{Start: 10, End: 20, Category: "sponsor"}}
	mediaPlayer := newLifecyclePlayer()
	client := &sponsorClient{snapshot: protocol.Snapshot{
		SelfID: "self", Room: protocol.Room{Mode: protocol.RoomCollaborative},
		Participants: []protocol.Participant{{ID: "self", Role: protocol.RoleMember}},
	}}
	state := player.State{PositionSeconds: 12, Paused: false, Rate: 1}
	if !service.skipSponsorBlock(context.Background(), client, mediaPlayer, state) {
		t.Fatal("SponsorBlock segment was not skipped")
	}
	if len(client.updates) != 1 || client.updates[0].PositionSeconds != 20 || !client.updates[0].Seek || !client.updates[0].SponsorBlock {
		t.Fatalf("unexpected SponsorBlock playback update: %#v", client.updates)
	}
	if service.skipSponsorBlock(context.Background(), client, mediaPlayer, state) {
		t.Fatal("SponsorBlock skipped the same segment twice")
	}
}

func TestTimelineSegmentsCombineChaptersAndSponsorBlock(t *testing.T) {
	service := New(context.Background(), nil)
	mediaPlayer := newLifecyclePlayer()
	mediaPlayer.state.DurationSeconds = 120
	mediaPlayer.state.Chapters = []player.Chapter{{StartSeconds: 0, Title: "Opening"}, {StartSeconds: 60, Title: "Act two"}}
	service.player = mediaPlayer
	service.sponsorSegments = []youtube.Segment{{Start: 30, End: 40, Category: "sponsor", Title: "Sponsor"}}

	segments := service.TimelineSegments()
	if len(segments) != 3 || segments[0].Kind != "chapter" || segments[1].Kind != "sponsorblock" || segments[2].EndSeconds != 120 {
		t.Fatalf("unexpected timeline segments: %#v", segments)
	}
}

func TestResolvedYouTubePlayerEventKeepsLogicalSource(t *testing.T) {
	service := New(context.Background(), nil)
	service.currentSource = "https://youtu.be/video"
	service.currentPlayerSource = "https://rr.example.googlevideo.com/signed-stream"
	service.sponsorSegments = []youtube.Segment{{Start: 1, End: 2}}

	got := service.logicalSourceForPlayerEvent("https://rr.example.googlevideo.com/signed-stream")
	if got != service.currentSource || got != "https://youtu.be/video" {
		t.Fatalf("resolved stream replaced logical YouTube source: %q", got)
	}
	if len(service.sponsorSegments) != 1 {
		t.Fatal("resolved stream event cleared SponsorBlock segments")
	}
}

func TestWaitForPlayerMediaWaitsForFirstDuration(t *testing.T) {
	service := New(context.Background(), nil)
	mediaPlayer := newLifecyclePlayer()
	mediaPlayer.state.Source = "movie.mkv"
	go func() {
		time.Sleep(60 * time.Millisecond)
		mediaPlayer.mu.Lock()
		mediaPlayer.state.DurationSeconds = 123
		mediaPlayer.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state := service.waitForPlayerMedia(ctx, mediaPlayer, 0)
	if state.DurationSeconds != 123 {
		t.Fatalf("published media before duration was ready: %#v", state)
	}
}

func TestMediaTransitionDoesNotBlockNextRoomCommand(t *testing.T) {
	service := New(context.Background(), nil)
	service.beginMediaTransition()
	service.endMediaTransition()

	service.mu.RLock()
	opening := service.openingMedia
	service.mu.RUnlock()
	if opening {
		t.Fatal("finished transition still blocks playback")
	}
}

func TestPauseQueuedDuringMediaTransitionWinsAtCompletion(t *testing.T) {
	service := New(context.Background(), nil)
	mediaPlayer := newLifecyclePlayer()
	mediaPlayer.state.Paused = false
	service.beginMediaTransition()
	if !service.queueTransitionPause(true) {
		t.Fatal("pause was not queued during media transition")
	}
	var published bool
	paused, err := service.finishMediaTransition(context.Background(), mediaPlayer, false, func(value bool) error {
		published = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := mediaPlayer.State(context.Background())
	if !paused || !published || !state.Paused {
		t.Fatalf("queued pause lost: result=%v published=%v state=%#v", paused, published, state)
	}
	if service.queueTransitionPause(false) {
		t.Fatal("completed transition still intercepted playback controls")
	}
}

func TestRemoteCorrectionDoesNotSwallowPlayerPause(t *testing.T) {
	s := New(context.Background(), nil)
	p := newLifecyclePlayer()
	s.lastPlayer = player.State{PositionSeconds: 10, Paused: false, Rate: 1, ObservedAt: time.Now()}
	tracked := &trackedPlayer{Player: p, service: s}
	if err := tracked.SetRate(context.Background(), 1.02); err != nil {
		t.Fatal(err)
	}
	current := s.lastPlayer
	current.Rate = 1.02
	if changed, _ := s.observeLocalChange(current); changed {
		t.Fatal("rate correction echoed as user intent")
	}
	current.Paused = true
	if changed, _ := s.observeLocalChange(current); !changed {
		t.Fatal("player pause swallowed during rate correction")
	}
}

func TestRemoteSeekEchoDoesNotSwallowNextUserSeek(t *testing.T) {
	s := New(context.Background(), nil)
	s.lastPlayer = player.State{PositionSeconds: 10, Paused: true, Rate: 1, ObservedAt: time.Now()}
	tracked := &trackedPlayer{Player: newLifecyclePlayer(), service: s}
	tracked.Seek(context.Background(), 20)
	state := s.lastPlayer
	state.PositionSeconds = 20
	if changed, _ := s.observeLocalChange(state); changed {
		t.Fatal("seek echo published")
	}
	state.PositionSeconds = 40
	if changed, seek := s.observeLocalChange(state); !changed || !seek {
		t.Fatal("subsequent player seek lost")
	}
}

func TestPlayerCloseCancelsMediaOperationOnly(t *testing.T) {
	s := New(context.Background(), nil)
	session, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()
	pctx, closePlayer := context.WithCancel(session)
	s.playerContext = pctx
	op, cancel := s.mediaOperationContext(session)
	defer cancel()
	closePlayer()
	select {
	case <-op.Done():
	case <-time.After(time.Second):
		t.Fatal("media operation stuck after close")
	}
	if session.Err() != nil {
		t.Fatal("room was disconnected with player")
	}
}

func TestBufferingRecoveryIsNotPublishedAsUserSeek(t *testing.T) {
	s := New(context.Background(), nil)
	s.lastPlayer = player.State{PositionSeconds: 10, Rate: 1, Buffering: true, ObservedAt: time.Now().Add(-10 * time.Second)}
	current := player.State{PositionSeconds: 10, Rate: 1, ObservedAt: time.Now()}
	if changed, seek := s.observeLocalChange(current); changed || seek {
		t.Fatal("buffer recovery reset the room's playback clock")
	}
}

func TestNoopCommandDoesNotSwallowNextResume(t *testing.T) {
	s := New(context.Background(), nil)
	p := newLifecyclePlayer()
	p.state.Paused = false
	tracked := &trackedPlayer{Player: p, service: s}
	tracked.SetPaused(context.Background(), false)
	s.lastPlayer = player.State{Paused: true, Rate: 1, ObservedAt: time.Now()}
	if changed, _ := s.observeLocalChange(player.State{Paused: false, Rate: 1, ObservedAt: time.Now()}); !changed {
		t.Fatal("a no-op command swallowed a real resume")
	}
}

func TestManualFileAfterYouTubeUsesNewIdentity(t *testing.T) {
	s := New(context.Background(), nil)
	s.currentSource = "https://youtu.be/video"
	s.currentPlayerSource = "https://cdn.example/signed"
	if got := s.logicalSourceForPlayerEvent("/tmp/movie.mkv"); got != "/tmp/movie.mkv" {
		t.Fatalf("new file mislabeled as YouTube: %s", got)
	}
}
