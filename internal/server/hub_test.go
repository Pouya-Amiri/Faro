package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
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
	if _, err := testJoin(h, testSession("one"), protocol.Hello{Name: "Ada", Room: "movie"}); err != nil {
		t.Fatal(err)
	}
	if _, err := testJoin(h, testSession("two"), protocol.Hello{Name: "Grace", Room: "movie"}); err == nil || !strings.Contains(err.Error(), "participant limit") {
		t.Fatalf("expected room limit, got %v", err)
	}
	if _, err := testJoin(h, testSession("three"), protocol.Hello{Name: "Linus", Room: "other"}); err == nil || !strings.Contains(err.Error(), "room limit") {
		t.Fatalf("expected server room limit, got %v", err)
	}
}

func TestRateWindowResets(t *testing.T) {
	limiter := rateWindow{maximum: 2, window: time.Second}
	now := time.Unix(10, 0)
	// Separate calls: allow has side effects, which staticcheck cannot see
	// through a repeated expression.
	first := limiter.allow(now)
	second := limiter.allow(now)
	third := limiter.allow(now)
	if !first || !second || third {
		t.Fatal("rate limit did not stop the third command")
	}
	if !limiter.allow(now.Add(time.Second)) {
		t.Fatal("rate limit did not reset after its window")
	}
}

func TestPlaybackCommandsEmitTypedActivity(t *testing.T) {
	h := newHub(1, 2)
	s := testSession("owner")
	joined, err := testJoin(h, s, protocol.Hello{Name: "Ada", Room: "movie"})
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
	joined, err := testJoin(h, s, protocol.Hello{Name: "Ada", Room: "movie"})
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
	owner, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := testJoin(h, testSession("member"), protocol.Hello{Name: "Grace", Room: "movie"})
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

func TestKickRespectsRolesAndClosesTheRemovedSession(t *testing.T) {
	h := newHub(1, 5)
	join := func(id, name string) *participant {
		t.Helper()
		s := testSession(id)
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		s.conn = local
		joined, err := testJoin(h, s, protocol.Hello{Name: name, Room: "movie"})
		if err != nil {
			t.Fatal(err)
		}
		return joined.participant
	}
	owner, moderator, other, member, watcher := join("owner", "Ada"), join("moderator", "Grace"), join("other", "Linus"), join("member", "Ken"), join("watcher", "Barbara")
	for _, p := range []*participant{moderator, other} {
		if err := h.setRole(owner, protocol.RoomRoleSet{ParticipantID: p.state.ID, Role: protocol.RoleModerator}); err != nil {
			t.Fatal(err)
		}
	}
	for _, denied := range []struct {
		actor, target *participant
	}{
		{member, watcher}, {moderator, owner}, {moderator, other}, {owner, owner},
	} {
		if err := h.kick(denied.actor, protocol.RoomParticipantKick{ParticipantID: denied.target.state.ID}); err == nil {
			t.Fatalf("%s removed %s", denied.actor.state.Name, denied.target.state.Name)
		}
	}
	for len(watcher.session.out) > 0 {
		<-watcher.session.out
	}
	if err := h.kick(moderator, protocol.RoomParticipantKick{ParticipantID: member.state.ID}); err != nil {
		t.Fatal(err)
	}
	if h.rooms["movie"].participants[member.state.ID] != nil {
		t.Fatal("removed participant is still in the room")
	}
	var kickedNotice bool
	for len(watcher.session.out) > 0 {
		frame := <-watcher.session.out
		if frame.envelope.Type != protocol.TypeActivityMessage {
			continue
		}
		activity, err := protocol.DecodePayload[protocol.ActivityMessage](frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
		kickedNotice = activity.Action == protocol.ActivityParticipantKick && activity.ParticipantName == "Grace" && activity.TargetName == "Ken"
	}
	if !kickedNotice {
		t.Fatal("the room was not told who removed whom")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case frame := <-member.session.out:
			if frame.envelope.Type != protocol.TypeError {
				continue
			}
			failure, err := protocol.DecodePayload[protocol.Error](frame.envelope)
			if err != nil {
				t.Fatal(err)
			}
			if failure.Code != protocol.ErrorKicked || !failure.Fatal {
				t.Fatalf("removed participant got %+v", failure)
			}
			frame.done <- nil
			select {
			case <-member.session.done:
			case <-time.After(time.Second):
				t.Fatal("removed participant's connection stayed open")
			}
			if err := h.kick(owner, protocol.RoomParticipantKick{ParticipantID: other.state.ID}); err != nil {
				t.Fatalf("owner could not remove a moderator: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("removed participant was not told why")
		}
	}
}

func TestSettingPlaylistDoesNotImplicitlyStartFirstItem(t *testing.T) {
	h := newHub(1, 2)
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
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
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{{ID: "one", Label: "One", URL: "https://example.com/one.mp4"}}}); err != nil {
		t.Fatal(err)
	}
	room := joined.room
	room.playback = protocol.Playback{Revision: 7, PositionSeconds: 481, Paused: false, Rate: 1.5, SetBy: "someone-else"}
	if err := h.selectPlaylist(joined.participant, protocol.PlaylistSelect{Index: 0}); err != nil {
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
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	one := protocol.PlaylistItem{ID: "one", Label: "One", URL: "https://example.com/one.mp4"}
	two := protocol.PlaylistItem{ID: "two", Label: "Two", URL: "https://example.com/two.mp4"}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{one, two}}); err != nil {
		t.Fatal(err)
	}
	if err := h.selectPlaylist(joined.participant, protocol.PlaylistSelect{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{two, one}}); err != nil {
		t.Fatal(err)
	}
	if got := joined.room.playlist.Selected; got != 1 {
		t.Fatalf("selected item moved to the wrong media after reorder: index %d", got)
	}
}

func TestPlaylistRemovalAndSourceReplacementResetPlayback(t *testing.T) {
	for _, replacement := range [][]protocol.PlaylistItem{
		nil,
		{{ID: "one", Label: "Other file", Media: &protocol.Media{Title: "Other file", Fingerprint: "file-v1:new"}}},
	} {
		h := newHub(1, 2)
		joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
		if err != nil {
			t.Fatal(err)
		}
		original := protocol.PlaylistItem{ID: "one", Label: "File", Media: &protocol.Media{Title: "File", Fingerprint: "file-v1:old"}}
		if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{original}}); err != nil {
			t.Fatal(err)
		}
		if err := h.selectPlaylist(joined.participant, protocol.PlaylistSelect{Index: 0}); err != nil {
			t.Fatal(err)
		}
		joined.room.playback = protocol.Playback{Revision: 7, PositionSeconds: 20, Paused: false, Rate: 1.5}
		if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: replacement}); err != nil {
			t.Fatal(err)
		}
		playback := joined.room.playback
		if !playback.Paused || playback.PositionSeconds != 0 || playback.Rate != 1 || playback.Revision != 8 {
			t.Fatalf("playlist source change retained playback: %#v", playback)
		}
	}
}

func TestPlaylistWheelUsesServerWinnerAndStartsItFromZero(t *testing.T) {
	h := newHub(1, 2)
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
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
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
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
	provider, err := testJoin(h, providerSession, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := testJoin(h, viewerSession, protocol.Hello{Name: "Grace", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := testJoin(h, legacySession, protocol.Hello{Name: "Linus", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", DurationSeconds: 3600, SizeBytes: 1 << 30, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := h.publishStreamOffer(provider.participant, protocol.MediaStreamOfferPublish{OfferID: "offer", Media: media}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := testSnapshot(h, viewer.participant); err != nil || len(snapshot.StreamOffers) != 1 {
		t.Fatalf("capable snapshot missing offer: snapshot=%#v err=%v", snapshot, err)
	}
	if snapshot, err := testSnapshot(h, legacy.participant); err != nil || snapshot.StreamOffers != nil {
		t.Fatalf("legacy snapshot exposed offer state: snapshot=%#v err=%v", snapshot, err)
	}
	for len(legacySession.out) > 0 {
		frame := <-legacySession.out
		if strings.HasPrefix(string(frame.envelope.Type), "media.stream.") {
			t.Fatalf("legacy session received stream event %q", frame.envelope.Type)
		}
	}

	if _, err := testRequestStream(h, viewer.participant, protocol.MediaStreamRequest{OfferID: "offer", ClientPublicKey: "ephemeral-public-key"}); err != nil {
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
	if snapshot, err := testSnapshot(h, viewer.participant); err != nil {
		t.Fatal(err)
	} else if raw, err := json.Marshal(snapshot); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(raw, []byte(secretBlob)) || bytes.Contains(raw, []byte(secretCapability)) {
		t.Fatalf("grant secret entered snapshot: %s", raw)
	}
}

func TestUnansweredStreamRequestExpiresSoonButGrantsKeepTheirTTL(t *testing.T) {
	previous := streamRequestTimeout
	streamRequestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { streamRequestTimeout = previous })
	policy := &protocol.MediaStreamPolicy{MaxOffersPerParticipant: 4, MaxViewersPerOffer: 5, GrantTTLSeconds: 120}
	h := newHub(1, 4, policy)
	providerSession, viewerSession := testStreamSession("provider"), testStreamSession("viewer")
	provider, err := testJoin(h, providerSession, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := testJoin(h, viewerSession, protocol.Hello{Name: "Grace", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", DurationSeconds: 3600, SizeBytes: 1 << 30, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := h.publishStreamOffer(provider.participant, protocol.MediaStreamOfferPublish{OfferID: "offer", Media: media}); err != nil {
		t.Fatal(err)
	}
	request := protocol.MediaStreamRequest{OfferID: "offer", ClientPublicKey: "ephemeral-public-key"}
	accepted, err := testRequestStream(h, viewer.participant, request)
	if err != nil {
		t.Fatal(err)
	}
	// The provider may still grant for the full TTL.
	if remaining := time.Until(time.UnixMilli(accepted.ExpiresAtUnixMs)); remaining < 100*time.Second {
		t.Fatalf("grant expiry shrank to %v", remaining)
	}
	if _, err := testRequestStream(h, viewer.participant, request); err == nil {
		t.Fatal("a second request was accepted while the first was pending")
	}
	// The provider never answers: the request is revoked and may be retried.
	deadline := time.After(2 * time.Second)
	for revoked := false; !revoked; {
		select {
		case frame := <-viewerSession.out:
			revoked = frame.envelope.Type == protocol.TypeStreamRevoked
		case <-deadline:
			t.Fatal("the unanswered request was never revoked")
		}
	}
	if _, err := testRequestStream(h, viewer.participant, request); err != nil {
		t.Fatalf("retry after expiry failed: %v", err)
	}
}

func TestPlaylistBaseRevisionRejectsStaleEdits(t *testing.T) {
	h := newHub(1, 2)
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	item := func(id string) protocol.PlaylistItem {
		return protocol.PlaylistItem{ID: id, Label: id, URL: "https://example.com/" + id + ".mp4"}
	}
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{item("one")}}); err != nil {
		t.Fatal(err)
	}
	base := joined.room.playlist.Revision
	// A friend's edit lands first.
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{item("one"), item("two")}, BaseRevision: &base}); err != nil {
		t.Fatal(err)
	}
	var failure *commandError
	err = h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: nil, BaseRevision: &base})
	if !errors.As(err, &failure) || failure.code != protocol.ErrorPlaylistConflict {
		t.Fatalf("stale edit returned %v, want a playlist conflict", err)
	}
	if len(joined.room.playlist.Items) != 2 {
		t.Fatalf("stale edit changed the playlist: %#v", joined.room.playlist.Items)
	}
	// Without a base revision the update applies, as for older clients.
	if err := h.setPlaylist(joined.participant, protocol.PlaylistSet{Items: []protocol.PlaylistItem{item("two")}}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectingPlaylistItemByID(t *testing.T) {
	h := newHub(1, 2)
	joined, err := testJoin(h, testSession("owner"), protocol.Hello{Name: "Ada", Room: "movie"})
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
	// The index is stale (the queue was reordered); the ID wins.
	if err := h.selectPlaylist(joined.participant, protocol.PlaylistSelect{Index: 0, ItemID: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := joined.room.playlist.Selected; got != 1 {
		t.Fatalf("selected %d, want the item with ID two", got)
	}
	var failure *commandError
	err = h.selectPlaylist(joined.participant, protocol.PlaylistSelect{Index: 0, ItemID: "gone"})
	if !errors.As(err, &failure) || failure.code != protocol.ErrorPlaylistItemNotFound || joined.room.playlist.Selected != 1 {
		t.Fatalf("selecting a removed item returned %v and selected %d", err, joined.room.playlist.Selected)
	}
}

// testJoin joins like a session does and returns what the welcome saw.
func testJoin(h *hub, s *session, hello protocol.Hello) (joinResult, error) {
	var result joinResult
	err := h.join(s, hello, func(joined joinResult) {
		s.participant = joined.participant
		result = joined
	})
	return result, err
}

// testRequestStream requests a stream and returns its acknowledgement.
func testRequestStream(h *hub, p *participant, request protocol.MediaStreamRequest) (protocol.MediaStreamRequestAccepted, error) {
	var accepted protocol.MediaStreamRequestAccepted
	err := h.requestStream(p, request, func(reply protocol.MediaStreamRequestAccepted) { accepted = reply })
	return accepted, err
}

func TestConcurrentChangesReachEveryoneInRevisionOrder(t *testing.T) {
	h := newHub(1, 4)
	sessions := []*session{testSession("one"), testSession("two"), testSession("observer")}
	for _, s := range sessions {
		s.out = make(chan outbound, 4096)
		if _, err := testJoin(h, s, protocol.Hello{Name: s.id, Room: "movie"}); err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	for _, s := range sessions[:2] {
		wait.Go(func() {
			for index := range 200 {
				if err := h.setPlayback(s.participant, protocol.PlaybackSet{PositionSeconds: float64(index), Rate: 1}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wait.Wait()
	observer := sessions[2]
	last, updates := uint64(0), 0
	for len(observer.out) > 0 {
		frame := <-observer.out
		if frame.envelope.Type != protocol.TypePlaybackUpdated {
			continue
		}
		playback, err := protocol.DecodePayload[protocol.Playback](frame.envelope)
		if err != nil {
			t.Fatal(err)
		}
		if playback.Revision <= last {
			t.Fatalf("playback revision %d arrived after %d", playback.Revision, last)
		}
		last = playback.Revision
		updates++
	}
	if updates != 400 {
		t.Fatalf("the observer saw %d of 400 playback updates", updates)
	}
}

func TestStreamRequestIsAcknowledgedBeforeTheProviderHearsOfIt(t *testing.T) {
	policy := &protocol.MediaStreamPolicy{MaxOffersPerParticipant: 4, MaxViewersPerOffer: 5, GrantTTLSeconds: 120}
	h := newHub(1, 4, policy)
	providerSession, viewerSession := testStreamSession("provider"), testStreamSession("viewer")
	provider, err := testJoin(h, providerSession, protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := testJoin(h, viewerSession, protocol.Hello{Name: "Grace", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: 1 << 30, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := h.publishStreamOffer(provider.participant, protocol.MediaStreamOfferPublish{OfferID: "offer", Media: media}); err != nil {
		t.Fatal(err)
	}
	for len(providerSession.out) > 0 {
		<-providerSession.out
	}
	err = h.requestStream(viewer.participant, protocol.MediaStreamRequest{OfferID: "offer", ClientPublicKey: "key"}, func(protocol.MediaStreamRequestAccepted) {
		for len(providerSession.out) > 0 {
			if frame := <-providerSession.out; frame.envelope.Type == protocol.TypeStreamRequested {
				t.Error("the provider was asked before the viewer's request was acknowledged")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	for len(providerSession.out) > 0 {
		asked = asked || (<-providerSession.out).envelope.Type == protocol.TypeStreamRequested
	}
	if !asked {
		t.Fatal("the provider was never asked for a grant")
	}
}

func TestStreamOffersMustHaveAPlausibleSize(t *testing.T) {
	policy := &protocol.MediaStreamPolicy{MaxOffersPerParticipant: 4, MaxViewersPerOffer: 5, GrantTTLSeconds: 120}
	h := newHub(1, 4, policy)
	provider, err := testJoin(h, testStreamSession("provider"), protocol.Hello{Name: "Ada", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	media := protocol.Media{Title: "Movie.mkv", SizeBytes: protocol.MaxStreamBytes + 1, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := h.publishStreamOffer(provider.participant, protocol.MediaStreamOfferPublish{OfferID: "offer", Media: media}); err == nil {
		t.Fatal("an impossibly large file was offered")
	}
}

// testSnapshot returns the snapshot p would receive on joining now.
func testSnapshot(h *hub, p *participant) (protocol.Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roomForLocked(p)
	if r == nil {
		return protocol.Snapshot{}, errors.New("not joined")
	}
	return r.snapshot(p.state.ID, p.session.supports(protocol.CapabilityMediaStreamV1)), nil
}
