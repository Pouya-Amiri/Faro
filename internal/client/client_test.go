package client

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/server"
)

type runningServer struct {
	address     string
	fingerprint string
	cancel      context.CancelFunc
	done        chan error
}

func startServer(t *testing.T, token string) runningServer {
	return startServerWithConfig(t, token, nil)
}

func startServerWithConfig(t *testing.T, token string, configure func(*server.Config)) runningServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.DefaultConfig()
	cfg.TLSDir = t.TempDir()
	cfg.JoinToken = token
	if configure != nil {
		configure(&cfg)
	}
	service, err := server.New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.ServeListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		service.Wait()
		<-done
	})
	return runningServer{address: listener.Addr().String(), fingerprint: service.Fingerprint(), cancel: cancel, done: done}
}

func connectClient(t *testing.T, running runningServer, name, room, token, ownerToken string) *Client {
	return connectClientWithCapabilities(t, running, name, room, token, ownerToken, nil)
}

func connectClientWithCapabilities(t *testing.T, running runningServer, name, room, token, ownerToken string, capabilities []protocol.Capability) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, Config{
		Address: running.address, Fingerprint: running.fingerprint,
		Name: name, Room: room, JoinToken: token, OwnerToken: ownerToken,
		ClientVersion: "test", Capabilities: capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestStreamingCapabilityIsExplicitAndServerGated(t *testing.T) {
	disabled := startServerWithConfig(t, "", func(cfg *server.Config) { cfg.StreamingEnabled = false })
	client := connectClientWithCapabilities(t, disabled, "Ada", "movie", "", "", []protocol.Capability{protocol.CapabilityMediaStreamV1})
	if client.Supports(protocol.CapabilityMediaStreamV1) || client.Welcome().Streaming != nil {
		t.Fatalf("disabled server negotiated streaming: %#v", client.Welcome())
	}

	enabled := startServerWithConfig(t, "", func(cfg *server.Config) { cfg.StreamingEnabled = true })
	capable := connectClientWithCapabilities(t, enabled, "Grace", "movie", "", "", []protocol.Capability{protocol.CapabilityMediaStreamV1})
	if !capable.Supports(protocol.CapabilityMediaStreamV1) || capable.Welcome().Streaming == nil {
		t.Fatalf("enabled server did not negotiate streaming: %#v", capable.Welcome())
	}
	legacy := connectClient(t, enabled, "Linus", "movie", "", "")
	if legacy.Supports(protocol.CapabilityMediaStreamV1) || legacy.Welcome().Streaming != nil || legacy.Snapshot().StreamOffers != nil {
		t.Fatalf("incapable client received streaming state: welcome=%#v snapshot=%#v", legacy.Welcome(), legacy.Snapshot())
	}
}

func TestStreamOfferRequestGrantAndWithdrawal(t *testing.T) {
	running := startServerWithConfig(t, "", func(cfg *server.Config) { cfg.StreamingEnabled = true })
	capabilities := []protocol.Capability{protocol.CapabilityMediaStreamV1}
	provider := connectClientWithCapabilities(t, running, "Ada", "movie", "", "", capabilities)
	viewer := connectClientWithCapabilities(t, running, "Grace", "movie", "", "", capabilities)
	media := protocol.Media{Title: "Movie.mkv", DurationSeconds: 7200, SizeBytes: 8 << 30, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}
	if err := provider.PublishStreamOffer(protocol.MediaStreamOfferPublish{OfferID: "offer-1", Media: media, MaxViewers: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, viewer, func(event Event) bool { return event.Type == protocol.TypeStreamOffersUpdated })
	if offers := viewer.Snapshot().StreamOffers; len(offers) != 1 || offers[0].Media.Fingerprint != media.Fingerprint || offers[0].ViewerCount != 0 {
		t.Fatalf("unexpected stream offers: %#v", offers)
	}
	accepted, err := viewer.RequestStream(protocol.MediaStreamRequest{OfferID: "offer-1", ClientPublicKey: "tailcat-client-public-key"})
	if err != nil {
		t.Fatal(err)
	}
	requested := waitFor(t, provider, func(event Event) bool { return event.StreamRequest != nil }).StreamRequest
	if requested.RequestID != accepted.RequestID || requested.OfferID != "offer-1" || requested.ViewerID != viewer.Welcome().ParticipantID {
		t.Fatalf("unexpected directed stream request: %#v", requested)
	}
	if err := provider.GrantStream(protocol.MediaStreamGrant{
		RequestID: requested.RequestID, ConnectionBlob: "opaque-tailcat-connection",
		TransferCapability: strings.Repeat("g", 32), ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	granted := waitFor(t, viewer, func(event Event) bool { return event.StreamGrant != nil }).StreamGrant
	if granted.RequestID != requested.RequestID || granted.ConnectionBlob == "" || granted.TransferCapability == "" {
		t.Fatalf("unexpected directed stream grant: %#v", granted)
	}
	if err := provider.WithdrawStreamOffer("offer-1"); err != nil {
		t.Fatal(err)
	}
	revoked := waitFor(t, viewer, func(event Event) bool { return event.StreamRevoked != nil }).StreamRevoked
	if revoked.RequestID != requested.RequestID || revoked.OfferID != "offer-1" {
		t.Fatalf("unexpected stream revocation: %#v", revoked)
	}
	if offers := viewer.Snapshot().StreamOffers; len(offers) != 0 {
		t.Fatalf("withdrawn offer remained in snapshot: %#v", offers)
	}
}

func TestConnectCreatesRoomAndOwnerCapability(t *testing.T) {
	running := startServer(t, "server-token-that-is-long-enough-1234")
	owner := connectClient(t, running, "Ada", "movie", "server-token-that-is-long-enough-1234", "")
	if owner.Welcome().OwnerToken == "" {
		t.Fatal("room creator did not receive an owner capability")
	}
	snapshot := owner.Snapshot()
	if snapshot.Room.ID != "movie" || len(snapshot.Participants) != 1 || snapshot.Participants[0].Role != protocol.RoleOwner {
		t.Fatalf("unexpected initial snapshot: %#v", snapshot)
	}
	if snapshot.Playlist.Items == nil {
		t.Fatal("new room playlist must be an empty array, not null")
	}
}

func TestModeratedRoomRejectsMemberPlayback(t *testing.T) {
	running := startServer(t, "")
	owner := connectClient(t, running, "Ada", "movie", "", "")
	member := connectClient(t, running, "Grace", "movie", "", "")
	if err := owner.SetRoomMode(protocol.RoomModeSet{Mode: protocol.RoomModerated}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, owner, func(event Event) bool { return event.Type == protocol.TypeRoomUpdated })
	if err := member.SetPlayback(protocol.PlaybackSet{PositionSeconds: 12, Paused: false, Rate: 1}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected acknowledged rejection, got %v", err)
	}
	if err := owner.SetPlayback(protocol.PlaybackSet{PositionSeconds: 12, Paused: false, Rate: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, member, func(event Event) bool { return event.Type == protocol.TypePlaybackUpdated })
	if got := member.Snapshot().Playback; got.SetBy != owner.Welcome().ParticipantID || got.Paused {
		t.Fatalf("unexpected playback state: %#v", got)
	}
}

func TestFingerprintMismatchIsFatal(t *testing.T) {
	running := startServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Config{
		Address: running.address, Fingerprint: strings.Repeat("0", 64),
		Name: "Ada", Room: "movie", ClientVersion: "test",
	})
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
}

func TestPlaylistWheelEventTracksOnlyActiveSpinInSnapshot(t *testing.T) {
	client := &Client{events: make(chan Event, 4), done: make(chan struct{})}
	wheel := protocol.PlaylistWheel{
		ID: "wheel", Phase: protocol.PlaylistWheelStarted,
		Items: []protocol.PlaylistWheelItem{{ID: "one", Label: "One"}, {ID: "two", Label: "Two"}}, Winner: 1,
	}
	envelope, err := protocol.NewEnvelope(protocol.TypePlaylistWheelUpdated, "", "", wheel)
	if err != nil {
		t.Fatal(err)
	}
	client.apply(envelope)
	if got := client.Snapshot().PlaylistWheel; got == nil || got.ID != wheel.ID || len(got.Items) != 2 {
		t.Fatalf("active wheel missing from snapshot: %#v", got)
	}
	wheel.Phase = protocol.PlaylistWheelCompleted
	envelope, err = protocol.NewEnvelope(protocol.TypePlaylistWheelUpdated, "", "", wheel)
	if err != nil {
		t.Fatal(err)
	}
	client.apply(envelope)
	if got := client.Snapshot().PlaylistWheel; got != nil {
		t.Fatalf("completed wheel remained active: %#v", got)
	}
}

func TestActivityMessageIsExposedAsTypedEvent(t *testing.T) {
	client := &Client{events: make(chan Event, 1), done: make(chan struct{})}
	activity := protocol.ActivityMessage{
		Action: protocol.ActivityPlaybackSeeked, ParticipantName: "Ada", PositionSeconds: 42, SentAtUnixMs: 1000,
	}
	envelope, err := protocol.NewEnvelope(protocol.TypeActivityMessage, "", "", activity)
	if err != nil {
		t.Fatal(err)
	}
	client.apply(envelope)
	event := <-client.Events()
	if event.Activity == nil || event.Activity.Action != protocol.ActivityPlaybackSeeked || event.Activity.PositionSeconds != 42 {
		t.Fatalf("unexpected activity event: %#v", event)
	}
}

func waitFor(t *testing.T, client *Client, predicate func(Event) bool) Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-client.Events():
			if predicate(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for client event")
		}
	}
}

func TestCustomTransportStillChecksPinnedTLS(t *testing.T) {
	running := startServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	cfg := Config{Address: running.address, Fingerprint: running.fingerprint, Name: "Peer", Room: "movie", ClientVersion: "test", DialContext: dial}
	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	cfg.Fingerprint = strings.Repeat("0", 64)
	if c, err := Connect(ctx, cfg); err == nil {
		c.Close()
		t.Fatal("custom transport bypassed certificate pin")
	}
}
