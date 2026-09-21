package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

func testSession(id string) *session {
	return &session{id: id, out: make(chan outbound, 32), done: make(chan struct{})}
}

func testStreamSession(id string) *session {
	s := testSession(id)
	s.capabilities = map[protocol.Capability]struct{}{protocol.CapabilityMediaStreamV1: {}}
	return s
}

func TestEmptyPlaylistSerializesAsArray(t *testing.T) {
	r := newRoom("movie", sha256.Sum256([]byte("owner")))
	raw, err := json.Marshal(r.snapshot(""))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"items":null`)) || !bytes.Contains(raw, []byte(`"items":[]`)) {
		t.Fatalf("empty playlist must serialize as an array: %s", raw)
	}
}

func TestHubEnforcesRoomAndParticipantLimits(t *testing.T) {
	h := newHub(1, 1)
	if _, err := h.join(testSession("one"), protocol.Hello{Name: "Ada", Room: "movie"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.join(testSession("two"), protocol.Hello{Name: "Grace", Room: "movie"}); err == nil || !strings.Contains(err.Error(), "participant limit") {
		t.Fatalf("expected room limit, got %v", err)
	}
	if _, err := h.join(testSession("three"), protocol.Hello{Name: "Linus", Room: "other"}); err == nil || !strings.Contains(err.Error(), "room limit") {
		t.Fatalf("expected server room limit, got %v", err)
	}
}

func TestRateWindowResets(t *testing.T) {
	limiter := rateWindow{maximum: 2, window: time.Second}
	now := time.Unix(10, 0)
	if !limiter.allow(now) || !limiter.allow(now) || limiter.allow(now) {
		t.Fatal("rate limit did not stop the third command")
	}
	if !limiter.allow(now.Add(time.Second)) {
		t.Fatal("rate limit did not reset after its window")
	}
}

func TestPlaybackCommandsEmitTypedActivity(t *testing.T) {
	h := newHub(1, 2)
	s := testSession("owner")
	joined, err := h.join(s, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setPlayback(joined.participant, protocol.PlaybackSet{PositionSeconds: 12, Paused: false, Rate: 1}); err != nil {
		t.Fatal(err)
	}
	var activity protocol.ActivityMessage
	for len(s.out) > 0 {
		frame := <-s.out
		if frame.envelope.Type != protocol.TypeActivityMessage {
			continue
		}
		activity, err = protocol.DecodePayload[protocol.ActivityMessage](frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
	}
	if activity.Action != protocol.ActivityPlaybackResumed || activity.ParticipantName != "Ada" || activity.PositionSeconds != 12 {
		t.Fatalf("unexpected playback activity: %#v", activity)
	}
}

func TestSponsorBlockSeekEmitsSystemActivity(t *testing.T) {
	h := newHub(1, 2)
	s := testSession("owner")
	joined, err := h.join(s, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setPlayback(joined.participant, protocol.PlaybackSet{PositionSeconds: 42, Paused: false, Rate: 1, Seek: true, SponsorBlock: true}); err != nil {
		t.Fatal(err)
	}
	var activity protocol.ActivityMessage
	for len(s.out) > 0 {
		frame := <-s.out
		if frame.envelope.Type != protocol.TypeActivityMessage {
			continue
		}
		activity, err = protocol.DecodePayload[protocol.ActivityMessage](frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
	}
	if activity.Action != protocol.ActivitySponsorSkipped || activity.ParticipantID != "" || activity.ParticipantName != "" {
		t.Fatalf("unexpected SponsorBlock activity: %#v", activity)
	}
}

func TestModeratedRoomKeepsAControllerWhenOwnerLeaves(t *testing.T) {
	h := newHub(2, 4)
	owner, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := h.join(testSession("member"), protocol.Hello{Name: "Grace", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setMode(owner.participant, protocol.RoomModeSet{Mode: protocol.RoomModerated}); err != nil {
		t.Fatal(err)
	}
	h.leave(owner.participant)
	if member.participant.state.Role != protocol.RoleModerator {
		t.Fatalf("remaining participant was not promoted: %s", member.participant.state.Role)
	}
}

func TestSettingPlaylistDoesNotImplicitlyStartFirstItem(t *testing.T) {
	h := newHub(1, 2)
	joined, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{{ID: "one", Label: "One", URL: "https://example.com/one.mp4"}}}); err != nil {
		t.Fatal(err)
	}
	if got := joined.room.playlist.Selected; got != -1 {
		t.Fatalf("adding a playlist implicitly selected item %d", got)
	}
}

func TestSelectingPlaylistItemResetsPlaybackClock(t *testing.T) {
	h := newHub(1, 2)
	joined, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{{ID: "one", Label: "One", URL: "https://example.com/one.mp4"}}}); err != nil {
		t.Fatal(err)
	}
	room := joined.room
	room.playback = protocol.Playback{Revision: 7, PositionSeconds: 481, Paused: false, Rate: 1.5, SetBy: "someone-else"}
	if err := h.selectPlaylist(joined.participant, 0); err != nil {
		t.Fatal(err)
	}
	if room.playback.PositionSeconds != 0 || !room.playback.Paused || room.playback.Rate != 1 {
		t.Fatalf("playlist selection retained old playback state: %#v", room.playback)
	}
	if room.playback.SetBy != joined.participant.state.ID || room.playback.Revision != 8 {
		t.Fatalf("playlist reset was not attributed and revised: %#v", room.playback)
	}
}

func TestPlaylistReorderKeepsSelectedItemIdentity(t *testing.T) {
	h := newHub(1, 2)
	joined, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	one := protocol.PlaylistItem{ID: "one", Label: "One", URL: "https://example.com/one.mp4"}
	two := protocol.PlaylistItem{ID: "two", Label: "Two", URL: "https://example.com/two.mp4"}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{one, two}}); err != nil {
		t.Fatal(err)
	}
	if err := h.selectPlaylist(joined.participant, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{two, one}}); err != nil {
		t.Fatal(err)
	}
	if got := joined.room.playlist.Selected; got != 1 {
		t.Fatalf("selected item moved to the wrong media after reorder: index %d", got)
	}
}

func TestPlaylistWheelUsesServerWinnerAndStartsItFromZero(t *testing.T) {
	h := newHub(1, 2)
	joined, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	items := []protocol.PlaylistItem{
		{ID: "one", Label: "One", URL: "https://example.com/one.mp4"},
		{ID: "two", Label: "Two", URL: "https://example.com/two.mp4"},
		{ID: "three", Label: "Three", URL: "https://example.com/three.mp4"},
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: items}); err != nil {
		t.Fatal(err)
	}
	if err := h.spinPlaylistWheel(joined.participant); err != nil {
		t.Fatal(err)
	}
	wheel := cloneWheel(joined.room.wheel)
	if wheel == nil || wheel.Phase != protocol.PlaylistWheelStarted || len(wheel.Items) != len(items) {
		t.Fatalf("unexpected active wheel: %#v", wheel)
	}
	if snapshot := joined.room.snapshot(joined.participant.state.ID); snapshot.PlaylistWheel == nil || snapshot.PlaylistWheel.ID != wheel.ID {
		t.Fatalf("active wheel missing from room snapshot: %#v", snapshot.PlaylistWheel)
	}
	h.finishPlaylistWheel(joined.room.state.ID, wheel.ID)
	if joined.room.wheel != nil {
		t.Fatal("completed wheel remained active")
	}
	if got := joined.room.playlist.Selected; got != wheel.Winner {
		t.Fatalf("selected index %d, want server winner %d", got, wheel.Winner)
	}
	if joined.room.playback.PositionSeconds != 0 || joined.room.playback.Paused || joined.room.playback.Rate != 1 {
		t.Fatalf("wheel winner did not start from zero: %#v", joined.room.playback)
	}
}

func TestPlaylistMutationCancelsActiveWheel(t *testing.T) {
	h := newHub(1, 2)
	joined, err := h.join(testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	items := []protocol.PlaylistItem{
		{ID: "one", Label: "One", URL: "https://example.com/one.mp4"},
		{ID: "two", Label: "Two", URL: "https://example.com/two.mp4"},
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: items}); err != nil {
		t.Fatal(err)
	}
	if err := h.spinPlaylistWheel(joined.participant); err != nil {
		t.Fatal(err)
	}
	timer := joined.room.wheelTimer
	if timer == nil {
		t.Fatal("active wheel has no managed completion timer")
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{items[1], items[0]}}); err != nil {
		t.Fatal(err)
	}
	if joined.room.wheel != nil {
		t.Fatal("queue mutation did not cancel active wheel")
	}
	if joined.room.wheelTimer != nil || timer.Stop() {
		t.Fatal("queue mutation left the wheel timer running")
	}
}

func TestStreamOffersAndSecretsAreCapabilityScoped(t *testing.T) {
	policy := &protocol.MediaStreamPolicy{MaxOffersPerParticipant: 4, MaxViewersPerOffer: 5, GrantTTLSeconds: 120}
	h := newHub(1, 4, policy)
	providerSession := testStreamSession("provider")
	viewerSession := testStreamSession("viewer")
	legacySession := testSession("legacy")
	provider, err := h.join(providerSession, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := h.join(viewerSession, protocol.Hello{Name: "Grace", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := h.join(legacySession, protocol.Hello{Name: "Linus", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", DurationSeconds: 3600, SizeBytes: 1 << 30, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := h.publishStreamOffer(provider.participant, protocol.MediaStreamOfferPublish{OfferID: "offer", Media: media}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := h.snapshot(viewer.participant); err != nil || len(snapshot.StreamOffers) != 1 {
		t.Fatalf("capable snapshot missing offer: snapshot=%#v err=%v", snapshot, err)
	}
	if snapshot, err := h.snapshot(legacy.participant); err != nil || snapshot.StreamOffers != nil {
		t.Fatalf("legacy snapshot exposed offer state: snapshot=%#v err=%v", snapshot, err)
	}
	for len(legacySession.out) > 0 {
		frame := <-legacySession.out
		if strings.HasPrefix(string(frame.envelope.Type), "media.stream.") {
			t.Fatalf("legacy session received stream event %q", frame.envelope.Type)
		}
	}

	if _, err := h.requestStream(viewer.participant, protocol.MediaStreamRequest{OfferID: "offer", ClientPublicKey: "ephemeral-public-key"}); err != nil {
		t.Fatal(err)
	}
	var requested protocol.MediaStreamRequested
	for len(providerSession.out) > 0 {
		frame := <-providerSession.out
		if frame.envelope.Type == protocol.TypeStreamRequested {
			requested, err = protocol.DecodePayload[protocol.MediaStreamRequested](frame.envelope)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if requested.RequestID == "" || requested.ViewerID != viewer.participant.state.ID {
		t.Fatalf("provider did not receive directed request: %#v", requested)
	}
	secretBlob := "secret-connection-blob"
	secretCapability := strings.Repeat("s", 32)
	if err := h.grantStream(provider.participant, protocol.MediaStreamGrant{
		RequestID: requested.RequestID, ConnectionBlob: secretBlob, TransferCapability: secretCapability,
		ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	for len(providerSession.out) > 0 {
		frame := <-providerSession.out
		raw, err := json.Marshal(frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secretBlob)) || bytes.Contains(raw, []byte(secretCapability)) {
			t.Fatalf("grant secret was broadcast back to provider: %s", raw)
		}
	}
	foundGrant := false
	for len(viewerSession.out) > 0 {
		frame := <-viewerSession.out
		if frame.envelope.Type != protocol.TypeStreamGranted {
			continue
		}
		grant, err := protocol.DecodePayload[protocol.MediaStreamGranted](frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
		foundGrant = grant.ConnectionBlob == secretBlob && grant.TransferCapability == secretCapability
	}
	if !foundGrant {
		t.Fatal("viewer did not receive the directed stream grant")
	}
	if snapshot, err := h.snapshot(viewer.participant); err != nil {
		t.Fatal(err)
	} else if raw, err := json.Marshal(snapshot); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(raw, []byte(secretBlob)) || bytes.Contains(raw, []byte(secretCapability)) {
		t.Fatalf("grant secret entered snapshot: %s", raw)
	}
}
