package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
	"github.com/Pouya-Amiri/Faro/internal/syncer"
)

type lifecyclePlayer struct {
	mu     sync.Mutex
	state  player.State
	events chan player.Event
}

func newLifecyclePlayer() *lifecyclePlayer {
	return &lifecyclePlayer{state: player.State{Paused: true, Rate: 1, ObservedAt: time.Now()}, events: make(chan player.Event, 4)}
}

func (p *lifecyclePlayer) Capabilities() player.Capabilities {
	return player.Capabilities{PlaybackRate: true, URLs: true}
}
func (p *lifecyclePlayer) State(context.Context) (player.State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, nil
}
func (p *lifecyclePlayer) SetPaused(_ context.Context, value bool) error {
	p.mu.Lock()
	p.state.Paused = value
	p.mu.Unlock()
	return nil
}
func (p *lifecyclePlayer) Seek(_ context.Context, value float64) error {
	p.mu.Lock()
	p.state.PositionSeconds = value
	p.mu.Unlock()
	return nil
}
func (p *lifecyclePlayer) SetRate(_ context.Context, value float64) error {
	p.mu.Lock()
	p.state.Rate = value
	p.mu.Unlock()
	return nil
}
func (p *lifecyclePlayer) Open(_ context.Context, source string) error {
	p.mu.Lock()
	p.state.Source = source
	p.state.Title = "Test media"
	p.state.DurationSeconds = 120
	p.state.ObservedAt = time.Now()
	p.mu.Unlock()
	return nil
}
func (p *lifecyclePlayer) Events() <-chan player.Event { return p.events }
func (p *lifecyclePlayer) Close() error                { return nil }

func TestEmbeddedServerCreatesLocalAndShareInvites(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: "127.0.0.1:0", PublicHost: "watch.example.test", Room: "movie", Protected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.StopServer)
	local, err := invite.Parse(status.LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := invite.Parse(status.ShareInvite)
	if err != nil {
		t.Fatal(err)
	}
	if local.JoinToken == "" || local.JoinToken != shared.JoinToken || local.Fingerprint != shared.Fingerprint {
		t.Fatalf("local and share invites do not describe one protected server: %#v %#v", local, shared)
	}
	if shared.Room != "movie" || !strings.HasPrefix(shared.Address, "watch.example.test:") {
		t.Fatalf("unexpected share invite: %#v", shared)
	}
}

func TestAppStreamsOfferedFileThroughFakeTransport(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	network := streamtransport.NewFakeNetwork()
	host := New(context.Background(), nil)
	viewer := New(context.Background(), nil)
	host.streamFactory, viewer.streamFactory = network, network
	status, err := host.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie",
		StreamingDERPMapURL: "https://derp.example.test/map.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	t.Cleanup(viewer.Shutdown)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	viewer.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if err := viewer.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Viewer", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	content := []byte(strings.Repeat("streamed-media-", 20000))
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.offerStream(path, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var offerID string
	for offerID == "" {
		snapshot, snapshotErr := viewer.Snapshot()
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		offers := snapshot.StreamOffers
		if len(offers) != 0 {
			offerID = offers[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewer did not receive stream offer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := viewer.StreamFromOffer(offerID); err != nil {
		t.Fatal(err)
	}
	var gatewayURL string
	for gatewayURL == "" {
		viewer.mu.RLock()
		if viewer.streamGateway != nil {
			gatewayURL = viewer.streamGateway.URL()
		}
		viewer.mu.RUnlock()
		if gatewayURL != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewer stream gateway did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request, err := http.NewRequest(http.MethodGet, gatewayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=1000-99999")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || string(body) != string(content[1000:100000]) {
		t.Fatalf("unexpected streamed response: status=%d bytes=%d", response.StatusCode, len(body))
	}
	viewer.mu.RLock()
	identity := cloneStreamMedia(viewer.streamIdentity)
	viewer.mu.RUnlock()
	if identity == nil || !strings.HasPrefix(identity.Fingerprint, "file-v1:") {
		t.Fatalf("viewer did not preserve file identity: %#v", identity)
	}
	if err := viewer.StopStreaming(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamInterruptedReconnectRestoresSyncWithoutPublishingDeadGateway(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	network := streamtransport.NewFakeNetwork()
	host, viewer := New(context.Background(), nil), New(context.Background(), nil)
	host.streamFactory, viewer.streamFactory = network, network
	status, err := host.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie",
		StreamingDERPMapURL: "https://derp.example.test/map.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	t.Cleanup(viewer.Shutdown)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	viewerPlayer := newLifecyclePlayer()
	viewer.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return viewerPlayer, nil }
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if err := viewer.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Viewer", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte(strings.Repeat("streamed-media-", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.SetPlaylist([]PlaylistInput{{Label: "Movie", Source: path}}); err != nil {
		t.Fatal(err)
	}
	if err := host.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}
	wait := func(message string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal(message)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait("viewer did not receive playlist", func() bool {
		snapshot, snapshotErr := viewer.Snapshot()
		return snapshotErr == nil && snapshot.Playlist.Selected == 0 && len(snapshot.Playlist.Items) == 1
	})
	snapshot, _ := host.Snapshot()
	itemID := snapshot.Playlist.Items[0].ID
	if err := host.OfferPlaylistStream(itemID); err != nil {
		t.Fatal(err)
	}
	var deadGatewayURL string
	wait("viewer did not start initial stream", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		if viewer.streamGateway == nil {
			return false
		}
		deadGatewayURL = viewer.streamGateway.URL()
		return viewer.sync != nil
	})
	viewer.mu.RLock()
	failed := viewer.client
	viewer.mu.RUnlock()
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	wait("viewer did not detach interrupted stream", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.client == nil && viewer.streamGateway == nil
	})
	if err := host.StopOfferingStream(); err != nil {
		t.Fatal(err)
	}
	wait("viewer did not reconnect", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.client != nil && viewer.client != failed
	})
	viewer.mu.RLock()
	controller := viewer.sync
	currentSource, currentPlayerSource := viewer.currentSource, viewer.currentPlayerSource
	selectedItem, selectedIdentity := viewer.selectedItem, viewer.selectedItemIdentity
	viewer.mu.RUnlock()
	if controller == nil {
		t.Fatal("reconnect left the existing player without a sync controller")
	}
	if currentSource != "" || currentPlayerSource != "" || selectedItem != "" || selectedIdentity != "" {
		t.Fatalf("reconnect retained dead stream state: source=%q playerSource=%q item=%q identity=%q", currentSource, currentPlayerSource, selectedItem, selectedIdentity)
	}
	viewerSnapshot, err := viewer.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, participant := range viewerSnapshot.Participants {
		if participant.ID == viewerSnapshot.SelfID && participant.Media != nil {
			t.Fatalf("reconnect published dead gateway identity: %#v", participant.Media)
		}
	}
	state, err := viewerPlayer.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Source != deadGatewayURL {
		t.Fatalf("test player source = %q, want interrupted gateway %q", state.Source, deadGatewayURL)
	}
	if err := host.OfferPlaylistStream(itemID); err != nil {
		t.Fatal(err)
	}
	wait("viewer did not reacquire stream after reconnect", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.streamGateway != nil
	})
	if err := host.Seek(37); err != nil {
		t.Fatal(err)
	}
	wait("remote seek was not applied after stream reacquisition", func() bool {
		state, stateErr := viewerPlayer.State(context.Background())
		return stateErr == nil && state.PositionSeconds == 37
	})
}

func TestOfferStreamRejectsConnectionWithoutSessionContext(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	service.streamFactory = streamtransport.NewFakeNetwork()
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.sessionCtx = nil
	service.mu.Unlock()
	if err := service.offerStream(path, 1); err == nil || !strings.Contains(err.Error(), "connection changed") {
		t.Fatalf("offerStream with detached session context returned %v", err)
	}
}

func TestLeaveRoomStopsEmbeddedServer(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host"}); err != nil {
		t.Fatal(err)
	}
	service.LeaveRoom()
	if service.ServerStatus().Running {
		t.Fatal("embedded server remained active after leaving its room")
	}
	if _, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "next"}); err != nil {
		t.Fatalf("hosting again after leaving failed: %v", err)
	}
}

func TestSelectedPlaylistItemReloadsWhenIdentityChanges(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)
	mediaPlayer := newLifecyclePlayer()
	service.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return mediaPlayer, nil }
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first.mkv")
	second := filepath.Join(t.TempDir(), "second.mkv")
	if err := os.WriteFile(first, []byte("first media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("different media"), 0o600); err != nil {
		t.Fatal(err)
	}
	const stableID = "same-item"
	if err := service.SetPlaylist([]PlaylistInput{{ID: stableID, Label: "Movie", Source: first}}); err != nil {
		t.Fatal(err)
	}
	if err := service.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}
	waitForSource := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			state, stateErr := mediaPlayer.State(context.Background())
			if stateErr == nil && state.Source == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("player source did not become %q; last state=%#v error=%v", want, state, stateErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForSource(first)
	if err := service.SetPlaylist([]PlaylistInput{{ID: stableID, Label: "Replacement", Source: second}}); err != nil {
		t.Fatal(err)
	}
	waitForSource(second)
}

func TestPlaylistStreamConnectsAutomaticallyAndStopsWhenRemoved(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	network := streamtransport.NewFakeNetwork()
	host, viewer := New(context.Background(), nil), New(context.Background(), nil)
	host.streamFactory, viewer.streamFactory = network, network
	status, err := host.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie",
		StreamingDERPMapURL: "https://derp.example.test/map.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	t.Cleanup(viewer.Shutdown)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	viewer.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if err := viewer.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Viewer", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte(strings.Repeat("streamed-media-", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	const nextURL = "https://example.test/next.mp4"
	if err := host.SetPlaylist([]PlaylistInput{
		{Label: "Movie", Source: path},
		{Label: "Next", URL: nextURL},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}
	waitForState := func(message string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal(message)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForState("viewer did not receive selected playlist item", func() bool {
		snapshot, snapshotErr := viewer.Snapshot()
		return snapshotErr == nil && snapshot.Playlist.Selected == 0 && len(snapshot.Playlist.Items) == 2
	})
	snapshot, _ := host.Snapshot()
	if err := host.OfferPlaylistStream(snapshot.Playlist.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	waitForState("viewer did not connect to the offered stream automatically", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.streamGateway != nil
	})
	if err := host.SetPlaylist([]PlaylistInput{{
		ID: snapshot.Playlist.Items[1].ID, Label: snapshot.Playlist.Items[1].Label,
		URL: snapshot.Playlist.Items[1].URL, Media: snapshot.Playlist.Items[1].Media,
	}}); err != nil {
		t.Fatal(err)
	}
	waitForState("removed queue file kept streaming", func() bool {
		host.mu.RLock()
		hostStopped := host.streamPublisher == nil
		host.mu.RUnlock()
		viewer.mu.RLock()
		viewerStopped := viewer.streamGateway == nil
		viewer.mu.RUnlock()
		return hostStopped && viewerStopped
	})
	viewer.mu.RLock()
	staleSource, staleSelection := viewer.currentSource, viewer.selectedItem
	viewer.mu.RUnlock()
	if staleSource != "" || staleSelection != "" {
		t.Fatalf("removed stream retained playback identity: source=%q selected=%q", staleSource, staleSelection)
	}
	// Re-add the same queue identity, stream it again, and stop the offer
	// explicitly. This covers the revocation path as well as removal cleanup.
	if err := host.SetPlaylist([]PlaylistInput{
		{ID: snapshot.Playlist.Items[1].ID, Label: snapshot.Playlist.Items[1].Label,
			URL: snapshot.Playlist.Items[1].URL, Media: snapshot.Playlist.Items[1].Media},
		{ID: snapshot.Playlist.Items[0].ID, Label: snapshot.Playlist.Items[0].Label,
			URL: snapshot.Playlist.Items[0].URL, Media: snapshot.Playlist.Items[0].Media},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.SelectPlaylist(1); err != nil {
		t.Fatal(err)
	}
	if err := host.OfferPlaylistStream(snapshot.Playlist.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	waitForState("viewer did not reconnect after the file was added back", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.streamGateway != nil
	})
	if err := host.StopOfferingStream(); err != nil {
		t.Fatal(err)
	}
	waitForState("viewer retained the explicitly stopped stream", func() bool {
		viewer.mu.RLock()
		defer viewer.mu.RUnlock()
		return viewer.streamGateway == nil && viewer.currentSource == "" && viewer.selectedItem == ""
	})
	if err := host.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}
	waitForState("viewer kept playing the removed stream instead of the next item", func() bool {
		viewer.mu.RLock()
		mediaPlayer := viewer.player
		viewer.mu.RUnlock()
		if mediaPlayer == nil {
			return false
		}
		state, stateErr := mediaPlayer.State(context.Background())
		return stateErr == nil && state.Source == nextURL
	})
}

func TestRemovedPlaylistOfferIsWithdrawnAfterPlayerWasClosed(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	network := streamtransport.NewFakeNetwork()
	host := New(context.Background(), nil)
	host.streamFactory = network
	status, err := host.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "movie",
		StreamingDERPMapURL: "https://derp.example.test/map.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte(strings.Repeat("streamed-media-", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.SetPlaylist([]PlaylistInput{{Label: "Movie", Source: path}}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot()
	if err := host.OfferPlaylistStream(snapshot.Playlist.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.playerDismissed = true
	host.mu.Unlock()
	if err := host.SetPlaylist(nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		host.mu.RLock()
		stopped := host.streamPublisher == nil && host.streamOfferID == "" && host.streamOfferItemID == ""
		host.mu.RUnlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed playlist item remained offered after the player was closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEmbeddedServerWildcardListenAddressUsesLocalhost(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: ":0", PublicHost: "watch.example.test", Room: "watch", Protected: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.StopServer)
	local, err := invite.Parse(status.LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(local.Address, "localhost:") {
		t.Fatalf("wildcard listener should produce localhost invite, got %s", local.Address)
	}
}

func TestEmbeddedServerSpecificAddressUsesActualHost(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	specificIP := firstNonLoopbackIPv4(t)
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: specificIP + ":0", PublicHost: specificIP, Room: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.StopServer)
	local, err := invite.Parse(status.LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(local.Address)
	if host != specificIP {
		t.Fatalf("specific-interface listener should produce %s invite, got %s", specificIP, local.Address)
	}
}

func TestEmbeddedServerSelfConnect(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	specificIP := firstNonLoopbackIPv4(t)
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced",
		ListenAddress: specificIP + ":0", PublicHost: specificIP, Room: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.StopServer)
	parsed, err := invite.Parse(status.LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := faroclient.Connect(ctx, faroclient.Config{
		Address: parsed.Address, Fingerprint: parsed.Fingerprint,
		Name: "self", Room: parsed.Room, ClientVersion: "test",
	})
	if err != nil {
		t.Fatalf("self-connect via localInvite (%s) failed: %v", parsed.Address, err)
	}
	defer client.Close()
	if client.Snapshot().SelfID == "" {
		t.Fatal("connected but received no SelfID")
	}
}

func TestPlayerStartsLazilyAndClosingItKeepsSessionConnected(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)

	fake := newLifecyclePlayer()
	starts := 0
	service.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) {
		starts++
		return fake, nil
	}
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Ada", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatalf("connecting eagerly started %d players", starts)
	}
	mediaPath := t.TempDir() + "/movie.mkv"
	if err := os.WriteFile(mediaPath, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.OpenMedia(mediaPath); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("opening media started %d players", starts)
	}
	fake.events <- player.Event{Kind: player.EventClosed}
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.mu.RLock()
		attached := service.player != nil
		service.mu.RUnlock()
		if !attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed player stayed attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := service.Snapshot(); err != nil {
		t.Fatalf("closing player disconnected the session: %v", err)
	}
	if err := service.SetPaused(false); err != nil {
		t.Fatalf("pressing play did not reopen the player: %v", err)
	}
	if starts != 2 {
		t.Fatalf("playing after close did not start a fresh player: starts=%d", starts)
	}
}

func TestIndexingSavedDirectoryStartsPlayerForSelectedRemoteItem(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host := New(context.Background(), nil)
	status, err := host.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	mediaPath := filepath.Join(directory, "movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("matching media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.SetPlaylist([]PlaylistInput{{Label: "Movie", Source: mediaPath}}); err != nil {
		t.Fatal(err)
	}
	if err := host.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}

	guest := New(context.Background(), nil)
	t.Cleanup(guest.Shutdown)
	guestPlayer := newLifecyclePlayer()
	starts := 0
	guest.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) {
		starts++
		return guestPlayer, nil
	}
	if err := guest.Connect(ConnectionRequest{Invite: status.ShareInvite, Name: "Guest", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatalf("guest player started before a matching local file was available: %d", starts)
	}
	if _, err := guest.IndexMediaDirectory(directory); err != nil {
		t.Fatal(err)
	}
	state, err := guestPlayer.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || state.Source != mediaPath {
		t.Fatalf("indexed selected item did not open: starts=%d state=%#v", starts, state)
	}
}

func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("cannot enumerate interfaces:", err)
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 interface found")
	return ""
}

func TestPlaylistAvailabilitySharedAndClosedSelectionWaitsForExplicitPlay(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host, guest := New(context.Background(), nil), New(context.Background(), nil)
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	guest.startPlayer = host.startPlayer
	status, err := host.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown()
	defer guest.Shutdown()
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host"}); err != nil {
		t.Fatal(err)
	}
	if err := guest.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Guest"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte("a matching file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := host.SetPlaylist([]PlaylistInput{{Source: path}}); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal("state did not converge")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool { snapshot, _ := guest.Snapshot(); return len(snapshot.Playlist.Items) == 1 })
	snapshot, _ := guest.Snapshot()
	id := snapshot.Playlist.Items[0].ID
	if guest.PlaylistAvailability()[id] {
		t.Fatal("guest incorrectly has host's file")
	}
	if !host.PlaylistAvailability()[id] {
		t.Fatal("host lost its file")
	}
	wait(func() bool {
		snapshot, _ := guest.Snapshot()
		for _, p := range snapshot.Participants {
			if p.Name == "Host" {
				return len(p.AvailableMedia) == 1
			}
		}
		return false
	})
	if err := host.SelectPlaylist(0); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { host.mu.RLock(); defer host.mu.RUnlock(); return host.selectedItem == id })
	host.mu.RLock()
	first := host.player
	host.mu.RUnlock()
	host.releasePlayer(first, "")
	// Room reconciliation must respect the user's decision to close the player.
	client, _ := host.connected()
	host.applySelectedPlaylist(context.Background(), client)
	host.mu.RLock()
	reopened := host.player
	host.mu.RUnlock()
	if reopened != nil {
		t.Fatal("selected playlist item reopened a player the user closed")
	}
	if err := host.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	host.mu.RLock()
	reopened = host.player
	host.mu.RUnlock()
	if reopened == nil || reopened == first {
		t.Fatal("explicit Play did not reopen the selected media")
	}
}

func TestPeerHostingInviteIntegration(t *testing.T) {
	if os.Getenv("FARO_TAILCAT_INTEGRATION") != "1" {
		t.Skip("set FARO_TAILCAT_INTEGRATION=1 for live peer hosting")
	}
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host, guest := New(context.Background(), nil), New(context.Background(), nil)
	defer host.Shutdown()
	defer guest.Shutdown()
	status, err := host.StartServer(ServerRequest{Room: "peer-test"})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := invite.Parse(status.ShareInvite)
	if err != nil || shared.Tailcat == "" || shared.JoinToken == "" {
		t.Fatalf("invalid peer invite: %v", err)
	}
	if err := host.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host"}); err != nil {
		t.Fatal(err)
	}
	if err := guest.Connect(ConnectionRequest{Invite: status.ShareInvite, Name: "Guest"}); err != nil {
		t.Fatal(err)
	}
	snap, err := guest.Snapshot()
	if err != nil || len(snap.Participants) != 2 {
		t.Fatalf("peer room did not connect: %v", err)
	}
}

func TestReconnectInvalidatesFailedConnectionBeforeBackoff(t *testing.T) {
	service := New(context.Background(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	failed := &faroclient.Client{}
	service.sessionCtx = ctx
	service.client = failed
	service.sync = &syncer.Controller{}

	done := make(chan struct{})
	go func() {
		service.reconnect(ctx, failed)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		client, controller := service.currentConnection()
		if client == nil && controller == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed client remained visible during reconnect backoff")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not stop after its session was cancelled")
	}
}
