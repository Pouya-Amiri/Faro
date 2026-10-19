package app

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/mediastream"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/server"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// scriptedRoom is a room server whose replies a test writes by hand, to
// produce orderings a real server makes rare, such as a grant arriving
// before the request's acknowledgement.
type scriptedRoom struct {
	mu    sync.Mutex
	codec *protocol.Codec
}

func (r *scriptedRoom) send(kind protocol.MessageType, replyTo string, payload any) {
	envelope, err := protocol.NewEnvelope(kind, "", replyTo, payload)
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.codec.Write(envelope)
}

// scriptedService connects a viewer to a scripted room offering offers.
// handle answers each command; unanswered commands time out.
func scriptedService(t *testing.T, offers []protocol.MediaStreamOffer, sink EventSink, handle func(*scriptedRoom, protocol.Envelope)) (*Service, *scriptedRoom) {
	t.Helper()
	identity, fingerprint, err := server.LoadOrCreateTLSIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", identity.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	room := &scriptedRoom{}
	ready := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
		room.codec = protocol.NewCodec(conn)
		hello, err := room.codec.Read()
		if err != nil {
			return
		}
		room.send(protocol.TypeWelcome, hello.ID, protocol.Welcome{
			SessionID: "script", ParticipantID: "viewer", RoomID: "room",
			Capabilities: []protocol.Capability{protocol.CapabilityMediaStreamV1},
			Streaming:    &protocol.MediaStreamPolicy{GrantTTLSeconds: 3600, MaxViewersPerOffer: 4, MaxOffersPerParticipant: 4},
		})
		room.send(protocol.TypeStateSnapshot, "", protocol.Snapshot{
			SelfID: "viewer", Room: protocol.Room{ID: "room", Mode: protocol.RoomCollaborative},
			Playlist: protocol.Playlist{Selected: -1, Items: []protocol.PlaylistItem{}},
			Playback: protocol.Playback{Rate: 1, Paused: true, UpdatedAtUnixMs: time.Now().UnixMilli()}, StreamOffers: offers,
		})
		close(ready)
		for {
			envelope, err := room.codec.Read()
			if err != nil {
				return
			}
			handle(room, envelope)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	client, err := faroclient.Connect(ctx, faroclient.Config{
		Address: listener.Addr().String(), Fingerprint: fingerprint, Name: "viewer", Room: "room",
		ClientVersion: "test", Capabilities: []protocol.Capability{protocol.CapabilityMediaStreamV1},
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	<-ready
	s := New(ctx, sink)
	s.client, s.sessionCtx, s.cancel = client, ctx, cancel
	s.autoOffer = false
	s.streamFactory = streamtransport.NewFakeNetwork()
	s.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	t.Cleanup(s.Shutdown)
	return s, room
}

// fakeProvider serves a file through a fake network, as a provider would.
type fakeProvider struct {
	network    *streamtransport.FakeNetwork
	publisher  streamtransport.Publisher
	source     *mediastream.Source
	media      protocol.Media
	capability string
}

func newFakeProvider(t *testing.T, size int64) *fakeProvider {
	t.Helper()
	path := filepath.Join(t.TempDir(), "movie.mkv")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	file.Close()
	media := protocol.Media{Title: "movie.mkv", SizeBytes: size, DurationSeconds: 120, Fingerprint: "file-v1:" + strings.Repeat("b", 64)}
	source, err := mediastream.NewSource(mediastream.SourceConfig{Path: path, Media: media})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	capability := strings.Repeat("c", 32)
	if err := source.Authorize(capability, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	network := streamtransport.NewFakeNetwork()
	publisher, err := network.StartPublisher(context.Background(), streamtransport.PublisherConfig{HandleConn: source.HandleConn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { publisher.Close() })
	return &fakeProvider{network: network, publisher: publisher, source: source, media: media, capability: capability}
}

func (p *fakeProvider) offer(id string) protocol.MediaStreamOffer {
	return protocol.MediaStreamOffer{ID: id, ProviderID: "provider", MaxViewers: 4, Media: p.media}
}

// grant admits request's viewer and returns the grant message for it.
func (p *fakeProvider) grant(t *testing.T, envelope protocol.Envelope, requestID, offerID string) protocol.MediaStreamGranted {
	request, err := protocol.DecodePayload[protocol.MediaStreamRequest](envelope)
	if err != nil {
		t.Error(err)
	}
	if err := p.publisher.AllowClient(request.ClientPublicKey); err != nil {
		t.Error(err)
	}
	return protocol.MediaStreamGranted{
		RequestID: requestID, OfferID: offerID, ProviderID: "provider", ConnectionBlob: p.publisher.ConnectionBlob(),
		TransferCapability: p.capability, ExpiresAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
	}
}

func acknowledge(room *scriptedRoom, envelope protocol.Envelope) {
	room.send(protocol.TypeCommandOK, envelope.ID, protocol.CommandOK{})
}

func currentStream(s *Service) *viewerStream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stream
}

func streamPhaseOf(s *Service) (streamPhase, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stream == nil {
		return 0, false
	}
	return s.stream.phase, true
}

// A request cancelled before its acknowledgement is withdrawn once the
// acknowledgement names it, instead of reserving the offer until it expires.
func TestRequestCancelledBeforeItsAcknowledgementIsWithdrawn(t *testing.T) {
	seen, release := make(chan struct{}), make(chan struct{})
	var revoked atomic.Value
	offer := protocol.MediaStreamOffer{ID: "offer", ProviderID: "provider", MaxViewers: 1, Media: protocol.Media{Title: "movie", SizeBytes: 8 << 20, Fingerprint: "file-v1:" + strings.Repeat("a", 64)}}
	s, _ := scriptedService(t, []protocol.MediaStreamOffer{offer}, nil, func(room *scriptedRoom, envelope protocol.Envelope) {
		switch envelope.Type {
		case protocol.TypeStreamRequest:
			close(seen)
			<-release
			room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: "late", OfferID: "offer"})
		case protocol.TypeStreamRevoke:
			value, _ := protocol.DecodePayload[protocol.MediaStreamRevoke](envelope)
			revoked.Store(value.RequestID)
			acknowledge(room, envelope)
		default:
			acknowledge(room, envelope)
		}
	})
	done := make(chan error, 1)
	go func() { done <- s.StreamFromOffer("offer") }()
	<-seen
	if err := s.StopStreaming(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("a cancelled request reported success")
	}
	waitFor(t, "the request acknowledged after cancellation was never withdrawn", func() bool { return revoked.Load() == "late" })
}

// The server may deliver a grant before the acknowledgement of its request;
// the late acknowledgement must not move an active stream back to
// "requesting" or withdraw the request.
func TestEarlyGrantIsNotUndoneByTheLateAcknowledgement(t *testing.T) {
	provider := newFakeProvider(t, 256*1024)
	release := make(chan struct{})
	var states []string
	var statesMu sync.Mutex
	var revokes atomic.Int32
	sink := func(event Event) {
		if event.Stream != nil && event.Stream.TotalBytes == 0 {
			statesMu.Lock()
			states = append(states, event.Stream.State)
			statesMu.Unlock()
		}
	}
	s, _ := scriptedService(t, []protocol.MediaStreamOffer{provider.offer("offer")}, sink, func(room *scriptedRoom, envelope protocol.Envelope) {
		switch envelope.Type {
		case protocol.TypeStreamRequest:
			room.send(protocol.TypeStreamGranted, "", provider.grant(t, envelope, "request", "offer"))
			go func() {
				<-release
				room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: "request", OfferID: "offer"})
			}()
		case protocol.TypeStreamRevoke:
			revokes.Add(1)
			acknowledge(room, envelope)
		default:
			acknowledge(room, envelope)
		}
	})
	s.streamFactory = provider.network
	go s.consumeClient(s.sessionCtx, s.client)
	done := make(chan error, 1)
	go func() { done <- s.StreamFromOffer("offer") }()
	waitFor(t, "the early grant never activated", func() bool { phase, ok := streamPhaseOf(s); return ok && phase == streamActive })
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	statesMu.Lock()
	defer statesMu.Unlock()
	if len(states) == 0 || states[len(states)-1] != "active" {
		t.Fatalf("the late acknowledgement changed the stream's state: %v", states)
	}
	if phase, ok := streamPhaseOf(s); !ok || phase != streamActive || revokes.Load() != 0 {
		t.Fatalf("the stream did not stay active (phase %v, present %v, %d revocations)", phase, ok, revokes.Load())
	}
}

// blockingFactory hands out viewers whose direct-path check waits until the
// activation is cancelled, to hold a stream in its activating phase.
type blockingFactory struct {
	*streamtransport.FakeNetwork
	entered chan struct{}
}

func (f blockingFactory) NewViewer() (streamtransport.Viewer, error) {
	viewer, err := f.FakeNetwork.NewViewer()
	return &blockingViewer{Viewer: viewer, entered: f.entered}, err
}

type blockingViewer struct {
	streamtransport.Viewer
	entered chan struct{}
}

func (v *blockingViewer) WaitForDirect(ctx context.Context) error {
	select {
	case v.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func activatingService(t *testing.T) (*Service, *scriptedRoom, *fakeProvider, chan struct{}) {
	t.Helper()
	provider := newFakeProvider(t, 256*1024)
	entered := make(chan struct{}, 1)
	s, room := scriptedService(t, []protocol.MediaStreamOffer{provider.offer("offer")}, nil, func(room *scriptedRoom, envelope protocol.Envelope) {
		switch envelope.Type {
		case protocol.TypeStreamRequest:
			room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: "request", OfferID: "offer"})
			room.send(protocol.TypeStreamGranted, "", provider.grant(t, envelope, "request", "offer"))
		default:
			acknowledge(room, envelope)
		}
	})
	s.streamFactory = blockingFactory{FakeNetwork: provider.network, entered: entered}
	go s.consumeClient(s.sessionCtx, s.client)
	if err := s.StreamFromOffer("offer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never started activating")
	}
	return s, room, provider, entered
}

// A revocation that arrives while the grant is still being connected
// cancels that activation.
func TestRevocationCancelsAStreamStillActivating(t *testing.T) {
	s, room, _, _ := activatingService(t)
	stream := currentStream(s)
	room.send(protocol.TypeStreamRevoked, "", protocol.MediaStreamRevoked{RequestID: "request", OfferID: "offer", Reason: "provider withdrew"})
	waitFor(t, "the revocation left the activation running", func() bool { return stream.ctx.Err() != nil && currentStream(s) == nil })
}

// Locating a local copy replaces the stream of the same file even while the
// stream is still activating.
func TestLocatingALocalCopyCancelsAStreamStillActivating(t *testing.T) {
	s, _, _, _ := activatingService(t)
	stream := currentStream(s)
	s.mu.Lock()
	media := stream.media
	s.mu.Unlock()
	// The scripted room has no queue, so retire the way LocatePlaylistItem
	// does for the matching file.
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.rememberSource(media.Fingerprint, path)
	s.mu.RLock()
	matching := s.stream != nil && s.stream.media.Fingerprint == media.Fingerprint
	s.mu.RUnlock()
	if !matching || !s.retireStream(s.client, stream, nil) || stream.ctx.Err() == nil {
		t.Fatal("the activating stream was not retired for the local copy")
	}
}

// Cleanup of a stream that is no longer current changes nothing: no state
// events, no pause, no media publication for the newer stream.
func TestRetiringAnObsoleteStreamLeavesTheCurrentOneAlone(t *testing.T) {
	var events atomic.Int32
	s, _ := scriptedService(t, nil, func(event Event) {
		if event.Stream != nil || event.Error != nil {
			events.Add(1)
		}
	}, acknowledge)
	ctx, cancel := context.WithCancel(context.Background())
	obsolete := &viewerStream{id: 1, ctx: ctx, cancel: cancel}
	current := &viewerStream{id: 2, ctx: context.Background(), cancel: func() {}, phase: streamActivating}
	s.mu.Lock()
	s.stream = current
	s.mu.Unlock()
	if s.retireStream(s.client, obsolete, &protocol.Error{Code: "stream_connect", Message: "old failure"}) {
		t.Fatal("an obsolete stream was retired as if it were current")
	}
	if currentStream(s) != current || events.Load() != 0 {
		t.Fatalf("an obsolete stream's failure affected the current one (%d events)", events.Load())
	}
}

// Retiring a stream that the player no longer shows does not pause the
// player or clear what it shows now.
func TestRetiringAStreamDoesNotPauseTheReplacement(t *testing.T) {
	provider := newFakeProvider(t, 256*1024)
	s, _ := scriptedService(t, nil, nil, acknowledge)
	viewer, err := provider.network.NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := mediastream.NewGateway(mediastream.GatewayConfig{
		Viewer: viewer, Capability: provider.capability, Media: provider.media, CacheBytes: 4 << 20, DisableBackgroundFill: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement := newLifecyclePlayer()
	replacement.state.Paused = false
	ctx, cancel := context.WithCancel(context.Background())
	stream := &viewerStream{id: 1, ctx: ctx, cancel: cancel, phase: streamActive, gateway: gateway}
	s.mu.Lock()
	s.stream, s.player, s.currentSource = stream, replacement, "/media/replacement.mkv"
	s.mu.Unlock()
	if !s.retireStream(s.client, stream, nil) {
		t.Fatal("the stream was not retired")
	}
	if state, _ := replacement.State(context.Background()); state.Paused {
		t.Fatal("retiring the old stream paused the replacement")
	}
	s.mu.RLock()
	source := s.currentSource
	s.mu.RUnlock()
	if source != "/media/replacement.mkv" {
		t.Fatalf("retiring the old stream cleared the replacement's source: %q", source)
	}
}

// startRoomService hosts a room and connects s to it.
func startRoomService(t *testing.T) *Service {
	t.Helper()
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	s := New(context.Background(), nil)
	s.autoOffer = false
	s.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	status, err := s.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", Room: "room", StreamingDERPMapURL: "https://derp.example.test/map.json"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	if err := s.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func playerSource(s *Service) string {
	s.mu.RLock()
	mediaPlayer := s.player
	s.mu.RUnlock()
	if mediaPlayer == nil {
		return ""
	}
	state, _ := mediaPlayer.State(context.Background())
	return state.Source
}

// failingOpenPlayer unloads what it played before failing to open bad.mp4,
// as mpv does with loadfile replace.
type failingOpenPlayer struct {
	*lifecyclePlayer
	failures atomic.Int32
}

func (p *failingOpenPlayer) Open(ctx context.Context, source string) error {
	if strings.Contains(source, "bad.mp4") {
		p.mu.Lock()
		p.state.Source, p.state.DurationSeconds = "", 0
		p.mu.Unlock()
		p.failures.Add(1)
		return errors.New("the replacement failed after the old media was unloaded")
	}
	return p.lifecyclePlayer.Open(ctx, source)
}

// When opening B unloads A and then fails, selecting A again reloads it.
func TestReturningAfterAFailedReplacementReloadsThePreviousItem(t *testing.T) {
	s := startRoomService(t)
	mediaPlayer := &failingOpenPlayer{lifecyclePlayer: newLifecyclePlayer()}
	s.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return mediaPlayer, nil }
	if err := s.SetPlaylist([]PlaylistInput{{URL: "https://example.test/good.mp4", Label: "good"}, {URL: "https://example.test/bad.mp4", Label: "bad"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first item never opened", func() bool { return playerSource(s) == "https://example.test/good.mp4" })
	if err := s.SelectPlaylist(1, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the replacement was never tried", func() bool { return mediaPlayer.failures.Load() > 0 })
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "returning to the first item did not reload it", func() bool { return playerSource(s) == "https://example.test/good.mp4" })
}

// blockingResolver holds a YouTube resolution until released.
type blockingResolver struct {
	entered, release chan struct{}
}

func (r *blockingResolver) Inspect(context.Context, string) (youtube.Info, error) {
	return youtube.Info{Title: "video", Duration: 120}, nil
}

func (r *blockingResolver) Resolve(ctx context.Context, _ string, _ int) (youtube.Stream, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return youtube.Stream{}, ctx.Err()
	}
	return youtube.Stream{CombinedURL: "https://cdn.example.test/old-video", Info: youtube.Info{Title: "old video", Duration: 120}}, nil
}

// A quality change still resolving when another item is selected gives way
// to the selection instead of reopening the old video over it.
func TestQualityChangeCannotOverwriteANewerSelection(t *testing.T) {
	s := startRoomService(t)
	source := "https://www.youtube.com/watch?v=abcdefghijk"
	resolver := &blockingResolver{entered: make(chan struct{}, 1), release: make(chan struct{})}
	if _, mediaPlayer, err := s.ensurePlayer(playerStartExplicit); err != nil {
		t.Fatal(err)
	} else if err := mediaPlayer.Open(context.Background(), "https://cdn.example.test/initial"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.currentSource, s.youtube = source, resolver
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.ChangeYouTubeQuality(source, 720) }()
	<-resolver.entered
	if err := s.SetPlaylist([]PlaylistInput{{URL: "https://example.test/new.mp4", Label: "new"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new selection did not open", func() bool { return playerSource(s) == "https://example.test/new.mp4" })
	close(resolver.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if source := playerSource(s); source != "https://example.test/new.mp4" {
		t.Fatalf("the old quality change replaced the new selection with %q", source)
	}
}

// Selecting another item does not wait for a slow load of the previous one.
func TestSelectingAnotherItemSupersedesASlowLoad(t *testing.T) {
	s := startRoomService(t)
	resolver := &blockingResolver{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(resolver.release) })
	s.mu.Lock()
	s.youtube = resolver
	s.mu.Unlock()
	if err := s.SetPlaylist([]PlaylistInput{{URL: "https://www.youtube.com/watch?v=abcdefghijk", Label: "slow"}, {URL: "https://example.test/next.mp4", Label: "next"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	<-resolver.entered
	if err := s.SelectPlaylist(1, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the next item waited for the slow one", func() bool { return playerSource(s) == "https://example.test/next.mp4" })
}

// Retiring an obsolete player automatically does not pause the room, which
// the current selection owns now; only a player the user closed does.
func TestAutomaticPlayerRetirementDoesNotPauseTheRoom(t *testing.T) {
	s := startRoomService(t)
	mediaPlayer := newLifecyclePlayer()
	s.mu.Lock()
	s.player, s.selectedItem = mediaPlayer, "old-item"
	client := s.client
	s.mu.Unlock()
	if err := client.SetPlayback(protocol.PlaybackSet{PositionSeconds: 30, Rate: 1}); err != nil {
		t.Fatal(err)
	}
	s.releasePlayer(mediaPlayer, "", false)
	if snapshot, _ := s.Snapshot(); snapshot.Playback.Paused {
		t.Fatal("retiring an obsolete player paused the room")
	}
	closed := newLifecyclePlayer()
	s.mu.Lock()
	s.player = closed
	s.mu.Unlock()
	s.releasePlayer(closed, "", true)
	waitFor(t, "closing the player did not pause the room", func() bool { snapshot, _ := s.Snapshot(); return snapshot.Playback.Paused })
}

// An older Connect still in progress cannot replace a newer one that
// already succeeded.
func TestEarlierConnectCannotReplaceALaterOne(t *testing.T) {
	host := startRoomService(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	parsed, err := invite.Parse(host.ServerStatus().LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		close(entered)
		<-release
		remote, err := net.Dial("tcp", parsed.Address)
		if err != nil {
			return
		}
		defer remote.Close()
		go func() { _, _ = io.Copy(peer, remote) }()
		_, _ = io.Copy(remote, peer)
	}()
	target := New(context.Background(), nil)
	t.Cleanup(target.Shutdown)
	delayed := parsed
	delayed.Address = listener.Addr().String()
	delayedInvite, err := invite.Format(delayed)
	if err != nil {
		t.Fatal(err)
	}
	older := make(chan error, 1)
	go func() {
		older <- target.Connect(ConnectionRequest{Invite: delayedInvite, Name: "older", Player: "mpv"})
	}()
	<-entered
	if err := target.Connect(ConnectionRequest{Invite: host.ServerStatus().LocalInvite, Name: "newer", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-older; !errors.Is(err, ErrConnectCancelled) {
		t.Fatalf("the older attempt ended with %v", err)
	}
	target.mu.RLock()
	name := target.request.Name
	target.mu.RUnlock()
	if name != "newer" {
		t.Fatalf("the older attempt replaced the newer connection: %q", name)
	}
}

// A remembered local copy that was moved away no longer blocks streaming.
func TestMovedLocalCopyDoesNotBlockStreaming(t *testing.T) {
	provider := newFakeProvider(t, 256*1024)
	s, _ := scriptedService(t, []protocol.MediaStreamOffer{provider.offer("offer")}, nil, func(room *scriptedRoom, envelope protocol.Envelope) {
		if envelope.Type == protocol.TypeStreamRequest {
			room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: "request", OfferID: "offer"})
			return
		}
		acknowledge(room, envelope)
	})
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.rememberSource(provider.media.Fingerprint, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.StreamFromOffer("offer"); err != nil {
		t.Fatalf("a moved local copy blocked streaming: %v", err)
	}
}

// After StopStreaming, offer updates do not start the same stream again
// until someone plays something.
func TestStoppedStreamStaysStoppedUntilPlayedAgain(t *testing.T) {
	provider := newFakeProvider(t, 256*1024)
	item := protocol.PlaylistItem{ID: "item", Label: "movie", Media: &provider.media}
	var requests atomic.Int32
	s, room := scriptedService(t, []protocol.MediaStreamOffer{provider.offer("offer")}, nil, func(room *scriptedRoom, envelope protocol.Envelope) {
		if envelope.Type == protocol.TypeStreamRequest {
			requests.Add(1)
			room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: "request", OfferID: "offer"})
			return
		}
		acknowledge(room, envelope)
	})
	room.send(protocol.TypePlaylistUpdated, "", protocol.Playlist{Revision: 1, Selected: 0, Items: []protocol.PlaylistItem{item}})
	waitFor(t, "the selection never arrived", func() bool { _, ok := selectedPlaylistItem(s.client.Snapshot().Playlist); return ok })
	s.reconcilePlaylistStreams(s.sessionCtx, s.client)
	waitFor(t, "the selected item was not requested", func() bool { return requests.Load() == 1 })
	if err := s.StopStreaming(); err != nil {
		t.Fatal(err)
	}
	s.reconcilePlaylistStreams(s.sessionCtx, s.client)
	time.Sleep(100 * time.Millisecond)
	if requests.Load() != 1 || currentStream(s) != nil {
		t.Fatal("the stopped stream was requested again without a new play intent")
	}
	s.resumeStreamingIntent()
	s.reconcilePlaylistStreams(s.sessionCtx, s.client)
	waitFor(t, "playing again did not request the stream", func() bool { return requests.Load() == 2 })
}

// openedThenClosedPlayer reports no duration, so a load waits for one, and
// signals when Open was called.
type openedThenClosedPlayer struct {
	*lifecyclePlayer
	opened chan struct{}
}

func (p *openedThenClosedPlayer) Open(ctx context.Context, source string) error {
	_ = p.lifecyclePlayer.Open(ctx, source)
	p.mu.Lock()
	p.state.DurationSeconds = 0
	p.mu.Unlock()
	close(p.opened)
	return nil
}

// A load whose player closed while it waited for the media's duration
// commits nothing afterwards.
func TestLoadIntoAClosedPlayerCommitsNothing(t *testing.T) {
	s := startRoomService(t)
	mediaPlayer := &openedThenClosedPlayer{lifecyclePlayer: newLifecyclePlayer(), opened: make(chan struct{})}
	s.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return mediaPlayer, nil }
	if err := s.SetPlaylist([]PlaylistInput{{URL: "https://example.test/unknown-duration.mp4", Label: "unknown"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectPlaylist(0, ""); err != nil {
		t.Fatal(err)
	}
	<-mediaPlayer.opened
	s.releasePlayer(mediaPlayer, "closed while loading", true)
	waitFor(t, "the interrupted load never finished", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.load != nil && s.load.done
	})
	s.mu.RLock()
	selected, current := s.selectedItem, s.player
	s.mu.RUnlock()
	snapshot, _ := s.Snapshot()
	published := false
	for _, participant := range snapshot.Participants {
		if participant.ID == snapshot.SelfID {
			published = participant.Media != nil
		}
	}
	if current != nil || selected != "" || published {
		t.Fatalf("a load into a closed player committed: selected=%q media=%v", selected, published)
	}
}

// When a grant ends before the file is cached, playback continues from the
// cache while a new grant is requested, and the same gateway carries on.
func TestStreamResumesThroughANewGrant(t *testing.T) {
	previous := streamResumeTimeout
	streamResumeTimeout = 5 * time.Second
	t.Cleanup(func() { streamResumeTimeout = previous })
	// Larger than the memory cache, so nothing downloads in the background
	// and the file stays incomplete.
	provider := newFakeProvider(t, 600<<20)
	var requests atomic.Int32
	s, room := scriptedService(t, []protocol.MediaStreamOffer{provider.offer("offer")}, nil, func(room *scriptedRoom, envelope protocol.Envelope) {
		switch envelope.Type {
		case protocol.TypeStreamRequest:
			id := []string{"first", "second"}[min(int(requests.Add(1))-1, 1)]
			room.send(protocol.TypeStreamRequestAccepted, envelope.ID, protocol.MediaStreamRequestAccepted{RequestID: id, OfferID: "offer"})
			room.send(protocol.TypeStreamGranted, "", provider.grant(t, envelope, id, "offer"))
		default:
			acknowledge(room, envelope)
		}
	})
	s.streamFactory = provider.network
	s.SetStreamCacheLimit(-1)
	go s.consumeClient(s.sessionCtx, s.client)
	if err := s.StreamFromOffer("offer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stream never became active", func() bool { phase, ok := streamPhaseOf(s); return ok && phase == streamActive })
	stream := currentStream(s)
	s.mu.RLock()
	gateway := stream.gateway
	s.mu.RUnlock()
	room.send(protocol.TypeStreamRevoked, "", protocol.MediaStreamRevoked{RequestID: "first", OfferID: "offer", Reason: "stream grant expired"})
	waitFor(t, "the stream did not resume through a new grant", func() bool {
		phase, ok := streamPhaseOf(s)
		return ok && phase == streamActive && requests.Load() == 2
	})
	s.mu.RLock()
	sameStream, sameGateway := s.stream == stream, stream.gateway == gateway
	s.mu.RUnlock()
	if !sameStream || !sameGateway || gateway.Progress().Route != mediastream.RouteDirect {
		t.Fatal("resuming replaced the stream instead of reconnecting its gateway")
	}
}
