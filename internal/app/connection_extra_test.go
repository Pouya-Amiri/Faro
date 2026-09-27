package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

func TestSeekWithoutPlayerPublishesRoomPosition(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	service := New(context.Background(), nil)
	status, err := service.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "seek"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Shutdown)
	if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: "Host", Player: "mpv"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Seek(42); err != nil {
		t.Fatalf("Seek without a player returned %v", err)
	}
	playback := service.client.Snapshot().Playback
	if playback.PositionSeconds != 42 || !playback.Seek || !playback.Paused {
		t.Fatalf("playback = %#v, want a paused seek to 42 s", playback)
	}
}

func TestSyncDriftIsReportedWhenItVisiblyChanges(t *testing.T) {
	var reports []SyncStatus
	service := New(context.Background(), func(event Event) {
		if event.Sync != nil {
			reports = append(reports, *event.Sync)
		}
	})
	for _, drift := range []float64{0.10, 0.12, 0.30, 0.31} {
		service.reportSync(SyncStatus{State: "catching-up", DriftSeconds: drift})
	}
	service.reportSync(SyncStatus{State: "synced", DriftSeconds: 0.31})
	want := []SyncStatus{{"catching-up", 0.10}, {"catching-up", 0.30}, {"synced", 0.31}}
	if len(reports) != len(want) {
		t.Fatalf("reports = %v, want %v", reports, want)
	}
	for index := range want {
		if reports[index] != want[index] {
			t.Fatalf("reports = %v, want %v", reports, want)
		}
	}
}

func TestCancelConnectStopsAHungAttempt(t *testing.T) {
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host := New(context.Background(), nil)
	status, err := host.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0", PublicHost: "localhost", Room: "hung"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Shutdown)
	// A server that accepts connections and never answers.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	parsed, err := invite.Parse(status.LocalInvite)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Address = silent.Addr().String()
	hung, err := invite.Format(parsed)
	if err != nil {
		t.Fatal(err)
	}
	guest := New(context.Background(), nil)
	t.Cleanup(guest.Shutdown)
	time.AfterFunc(200*time.Millisecond, guest.CancelConnect)
	started := time.Now()
	err = guest.Connect(ConnectionRequest{Invite: hung, Name: "Guest", Player: "mpv"})
	if !errors.Is(err, ErrConnectCancelled) {
		t.Fatalf("Connect returned %v, want ErrConnectCancelled", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancelling took %s", elapsed)
	}
	if guest.InRoom() {
		t.Fatal("a cancelled attempt left the service in a room")
	}
}

// durationlessPlayer never learns the media's duration, like a player whose
// file has not been probed yet.
type durationlessPlayer struct{ *lifecyclePlayer }

func (p durationlessPlayer) State(ctx context.Context) (player.State, error) {
	state, err := p.lifecyclePlayer.State(ctx)
	state.DurationSeconds = 0
	return state, err
}

func TestStreamViewerUsesItsPlayersDurationWhenTheOfferHasNone(t *testing.T) {
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
	host.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) {
		return durationlessPlayer{newLifecyclePlayer()}, nil
	}
	viewer.startPlayer = func(context.Context, ConnectionRequest) (player.Player, error) { return newLifecyclePlayer(), nil }
	for name, service := range map[string]*Service{"Host": host, "Viewer": viewer} {
		if err := service.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: name, Player: "mpv"}); err != nil {
			t.Fatal(err)
		}
	}
	// Not a real video, so no duration can be probed from the file either.
	path := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(path, []byte(strings.Repeat("streamed-media-", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.AppendPlaylist([]PlaylistInput{{Label: "Movie", Source: path}}, true); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := host.Snapshot()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err = host.OfferPlaylistStream(snapshot.Playlist.Items[0].ID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		viewerSnapshot, _ := viewer.Snapshot()
		for _, participant := range viewerSnapshot.Participants {
			if participant.ID == viewerSnapshot.SelfID && participant.Media != nil && participant.Media.DurationSeconds == 120 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the streaming viewer never published its player's duration: %#v", viewerSnapshot.Participants)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
