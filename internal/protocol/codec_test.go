package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type bufferStream struct{ bytes.Buffer }

func TestCodecRoundTrip(t *testing.T) {
	stream := &bufferStream{}
	codec := NewCodec(stream)
	want, err := NewEnvelope(TypePing, "request-1", "", Ping{ClientTimeUnixMs: 42})
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(want); err != nil {
		t.Fatal(err)
	}
	got, err := codec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypePing || got.ID != "request-1" {
		t.Fatalf("unexpected envelope: %#v", got)
	}
	payload, err := DecodePayload[Ping](got)
	if err != nil || payload.ClientTimeUnixMs != 42 {
		t.Fatalf("unexpected payload: %#v, %v", payload, err)
	}
}

func TestCodecRejectsWrongVersion(t *testing.T) {
	codec := NewCodec(&bufferStream{Buffer: *bytes.NewBufferString(`{"version":1,"type":"hello"}` + "\n")})
	if _, err := codec.Read(); err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodecRejectsLargeFrame(t *testing.T) {
	stream := &bufferStream{Buffer: *bytes.NewBuffer(make([]byte, MaxFrameSize+2))}
	stream.WriteByte('\n')
	_, err := NewCodec(stream).Read()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestCodecLimitsEachDirectionSeparately(t *testing.T) {
	large := Envelope{Version: Version, Type: TypeStateSnapshot, Payload: json.RawMessage(`"` + strings.Repeat("x", MaxFrameSize) + `"`)}
	stream := &bufferStream{}
	if err := NewCodec(stream).Write(large); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("default codec wrote a %d-byte frame: %v", MaxFrameSize, err)
	}
	if err := NewCodecWithLimits(stream, MaxFrameSize, MaxStateFrameSize).Write(large); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCodecWithLimits(stream, MaxStateFrameSize, MaxFrameSize).Read(); err != nil {
		t.Fatalf("state-sized read failed: %v", err)
	}
}
