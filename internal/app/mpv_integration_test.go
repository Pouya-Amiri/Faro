package app

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
)

// Run with FARO_MPV_INTEGRATION=1 to exercise actual IPC and player events.
func TestMPVRoomPauseSeekAndRecovery(t *testing.T) {
	t.Run("local", func(t *testing.T) { testMPVRoom(t, false) })
	t.Run("streamed", func(t *testing.T) { testMPVRoom(t, true) })
}

func testMPVRoom(t *testing.T, streamed bool) {
	if os.Getenv("FARO_MPV_INTEGRATION") != "1" {
		t.Skip("set FARO_MPV_INTEGRATION=1 to run real mpv")
	}
	t.Setenv("FARO_TLS_DIR", t.TempDir())
	host, guest := New(context.Background(), func(e Event) {
		if e.Activity != nil {
			t.Logf("activity: %+v", e.Activity)
		}
		if e.Error != nil {
			t.Logf("error: %+v", e.Error)
		}
	}), New(context.Background(), nil)
	network := streamtransport.NewFakeNetwork()
	host.streamFactory, guest.streamFactory = network, network
	status, err := host.StartServer(ServerRequest{Mode: "advanced", ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown()
	defer guest.Shutdown()
	for i, s := range []*Service{host, guest} {
		name := "Host"
		if i == 1 {
			name = "Guest"
		}
		if err := s.Connect(ConnectionRequest{Invite: status.LocalInvite, Name: name, Player: "mpv", PlayerArgs: []string{"--vo=null", "--ao=null", "--no-config"}}); err != nil {
			t.Fatal(err)
		}
	}
	// A generated PCM fixture avoids network/media codec dependencies.
	data := make([]byte, 44+8000*2*60)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 16000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	path := filepath.Join(t.TempDir(), "silence.wav")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := host.OpenMedia(path); err != nil {
		t.Fatal(err)
	}
	if streamed {
		if err := host.offerStream(path, 1); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var offerID string
		for offerID == "" {
			snap, _ := guest.Snapshot()
			if len(snap.StreamOffers) > 0 {
				offerID = snap.StreamOffers[0].ID
			}
			if time.Now().After(deadline) {
				t.Fatal("no offer")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := guest.StreamFromOffer(offerID); err != nil {
			t.Fatal(err)
		}
		for {
			guest.mu.RLock()
			active := guest.streamGateway != nil
			guest.mu.RUnlock()
			if active {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("stream did not activate")
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else if err := guest.OpenMedia(path); err != nil {
		t.Fatal(err)
	}
	state := func(s *Service) player.State {
		_, p, err := s.connectedPlayer()
		if err != nil {
			return player.State{}
		}
		value, _ := p.State(context.Background())
		return value
	}
	wait := func(label string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not converge: host=%+v guest=%+v", label, state(host), state(guest))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	_, hp, _ := host.connectedPlayer()
	if err := hp.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	wait("player pause", func() bool { snap, _ := guest.Snapshot(); return snap.Playback.Paused && state(guest).Paused })
	if err := hp.Seek(context.Background(), 25); err != nil {
		t.Fatal(err)
	}
	wait("player seek", func() bool { value := state(guest).PositionSeconds; return value > 24.5 && value < 25.5 })
	if err := hp.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	wait("player resume", func() bool { snap, _ := guest.Snapshot(); return !snap.Playback.Paused && !state(guest).Paused })
	hp.Close()
	wait("player release", func() bool { host.mu.RLock(); defer host.mu.RUnlock(); return host.player == nil })
	if err := host.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	wait("reopen", func() bool { host.mu.RLock(); defer host.mu.RUnlock(); return host.player != nil && host.player != hp })
}
