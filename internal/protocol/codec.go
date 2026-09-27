package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

// MaxFrameSize bounds frames a client sends: commands, chat and the queue.
const MaxFrameSize = 256 * 1024

// MaxStateFrameSize bounds frames the server sends. Room state it has
// already accepted (a full queue, many participants, each with up to 500
// available files) can far exceed MaxFrameSize, and failing to deliver it
// would disconnect everyone in the room.
const MaxStateFrameSize = 8 * 1024 * 1024

var ErrFrameTooLarge = errors.New("protocol frame is too large")

type Codec struct {
	reader     *bufio.Reader
	writer     io.Writer
	readLimit  int
	writeLimit int
	mu         sync.Mutex
}

// NewCodec limits frames in both directions to MaxFrameSize.
func NewCodec(stream io.ReadWriter) *Codec {
	return NewCodecWithLimits(stream, MaxFrameSize, MaxFrameSize)
}

// NewCodecWithLimits sets separate limits for frames read and written.
func NewCodecWithLimits(stream io.ReadWriter, readLimit, writeLimit int) *Codec {
	return &Codec{reader: bufio.NewReaderSize(stream, 32*1024), writer: stream, readLimit: readLimit, writeLimit: writeLimit}
}

func (c *Codec) Read() (Envelope, error) {
	frame, err := c.readFrame()
	if err != nil {
		return Envelope{}, err
	}
	if !utf8.Valid(frame) {
		return Envelope{}, errors.New("protocol frame is not UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode protocol frame: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Envelope{}, errors.New("protocol frame contains trailing JSON")
	}
	if envelope.Version != Version {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", envelope.Version)
	}
	if envelope.Type == "" {
		return Envelope{}, errors.New("protocol message type is required")
	}
	return envelope, nil
}

func (c *Codec) Write(envelope Envelope) error {
	if envelope.Version == 0 {
		envelope.Version = Version
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode protocol frame: %w", err)
	}
	if len(data) > c.writeLimit {
		return ErrFrameTooLarge
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.writer.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write protocol frame: %w", err)
	}
	return nil
}

func (c *Codec) readFrame() ([]byte, error) {
	var frame []byte
	for {
		part, err := c.reader.ReadSlice('\n')
		if len(frame)+len(part) > c.readLimit+1 {
			return nil, ErrFrameTooLarge
		}
		frame = append(frame, part...)
		if err == nil {
			return bytes.TrimSuffix(frame, []byte{'\n'}), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}
