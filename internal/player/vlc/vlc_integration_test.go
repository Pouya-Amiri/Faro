package vlc

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Run with FARO_VLC_INTEGRATION=1 go test ./internal/player/vlc -run Integration.
// It is opt-in because it launches the locally installed VLC executable.
func TestVLCIntegrationOpenPauseSeekAndRate(t *testing.T) {
	if os.Getenv("FARO_VLC_INTEGRATION") != "1" {
		t.Skip("set FARO_VLC_INTEGRATION=1 to launch VLC")
	}
	media := filepath.Join(t.TempDir(), "Faro smoke test.wav")
	if err := writeSilentWAV(media, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	instance, err := Start(ctx, Config{ExtraArgs: []string{"--intf=dummy", "--vout=dummy", "--aout=dummy"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err := instance.Open(ctx, media); err != nil {
		t.Fatal(err)
	}
	state, err := instance.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Paused || state.DurationSeconds < 3 {
		t.Fatalf("unexpected state after open: %+v", state)
	}
	if err := instance.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	state, err = instance.State(ctx)
	if err != nil || !state.Paused {
		t.Fatalf("pause was not observed: state=%+v err=%v", state, err)
	}
	if err := instance.Seek(ctx, 1.25); err != nil {
		t.Fatal(err)
	}
	state, err = instance.State(ctx)
	if err != nil || state.PositionSeconds < 0.75 || state.PositionSeconds > 1.75 {
		t.Fatalf("seek was not observed accurately: state=%+v err=%v", state, err)
	}
	if instance.Capabilities().PlaybackRate {
		if err := instance.SetRate(ctx, 1.1); err != nil {
			t.Fatal(err)
		}
	}
	if err := instance.Open(ctx, filepath.Join(t.TempDir(), "missing.mkv")); err == nil {
		t.Fatal("VLC reported a missing media file as successfully loaded")
	}
	state, err = instance.State(ctx)
	if err != nil || state.Source != "" || !state.Paused {
		t.Fatalf("failed load left stale media state: state=%+v err=%v", state, err)
	}
}

func writeSilentWAV(path string, duration time.Duration) error {
	const sampleRate = uint32(8000)
	dataSize := uint32(duration.Seconds() * float64(sampleRate) * 2)
	data := make([]byte, 44+dataSize)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], 36+dataSize)
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], sampleRate)
	binary.LittleEndian.PutUint32(data[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], dataSize)
	return os.WriteFile(path, data, 0o600)
}
