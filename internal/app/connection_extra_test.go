package app

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/invite"
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
