package mediaid

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func box(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], kind)
	return append(out, body...)
}

func mvhd(version byte, timescale uint32, duration uint64) []byte {
	payload := []byte{version, 0, 0, 0}
	if version == 1 {
		payload = append(payload, make([]byte, 16)...)
		payload = binary.BigEndian.AppendUint32(payload, timescale)
		payload = binary.BigEndian.AppendUint64(payload, duration)
	} else {
		payload = append(payload, make([]byte, 8)...)
		payload = binary.BigEndian.AppendUint32(payload, timescale)
		payload = binary.BigEndian.AppendUint32(payload, uint32(duration))
	}
	return box("mvhd", append(payload, make([]byte, 80)...))
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeDurationMP4(t *testing.T) {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	mdat := box("mdat", make([]byte, 4096))
	// moov after mdat, as written by most encoders without faststart.
	path := writeTemp(t, "clip.mp4", bytes.Join([][]byte{ftyp, mdat, box("moov", mvhd(0, 1000, 5_025_500))}, nil))
	if got := ProbeDuration(path); math.Abs(got-5025.5) > 1e-9 {
		t.Fatalf("mp4 duration = %v, want 5025.5", got)
	}
	path = writeTemp(t, "long.mov", bytes.Join([][]byte{ftyp, box("moov", mvhd(1, 90000, 90000*7200)), mdat}, nil))
	if got := ProbeDuration(path); got != 7200 {
		t.Fatalf("version 1 mvhd duration = %v, want 7200", got)
	}
}

func ebml(id uint64, payload []byte) []byte {
	var out []byte
	for shift := 24; shift >= 0; shift -= 8 {
		if part := byte(id >> shift); part != 0 || len(out) > 0 {
			out = append(out, part)
		}
	}
	// Eight-byte size: marker 0x01 followed by seven size bytes.
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(payload)))
	size[0] = 0x01
	return append(append(out, size...), payload...)
}

func TestProbeDurationMatroska(t *testing.T) {
	header := ebml(0x1A45DFA3, ebml(0x4282, []byte("webm")))
	duration := make([]byte, 8)
	binary.BigEndian.PutUint64(duration, math.Float64bits(1_234_500)) // ms at the default scale
	info := ebml(ebmlInfo, append(ebml(ebmlTimecodeScale, []byte{0x0F, 0x42, 0x40}), ebml(ebmlDuration, duration)...))
	seekHead := ebml(0x114D9B74, make([]byte, 32))
	// An unknown-size segment, as live muxers write it.
	segment := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, append(seekHead, info...)...)
	path := writeTemp(t, "show.mkv", append(header, segment...))
	if got := ProbeDuration(path); math.Abs(got-1234.5) > 1e-9 {
		t.Fatalf("matroska duration = %v, want 1234.5", got)
	}
}

func TestProbeDurationRejectsGarbage(t *testing.T) {
	for _, name := range []string{"broken.mp4", "broken.mkv"} {
		path := writeTemp(t, name, []byte("not a media file at all"))
		if got := ProbeDuration(path); got != 0 && got != ffprobeDuration(path) {
			t.Fatalf("%s: got %v for a file that is not media", name, got)
		}
	}
}
