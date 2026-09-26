package mediaid

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProbeDuration returns a local media file's duration in seconds, or 0 when it
// cannot be determined. MP4/QuickTime and Matroska/WebM headers are read
// directly; other formats use ffprobe when it is installed.
func ProbeDuration(path string) float64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	var duration float64
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov", ".m4a", ".3gp":
		duration, err = isoDuration(file, info.Size())
	case ".mkv", ".webm", ".mka":
		duration, err = matroskaDuration(file, info.Size())
	default:
		err = errUnknownFormat
	}
	if err == nil && duration > 0 && !math.IsInf(duration, 0) && !math.IsNaN(duration) {
		return duration
	}
	return ffprobeDuration(path)
}

var errUnknownFormat = errors.New("unsupported container")

// isoDuration reads moov/mvhd from an ISO base media file. The moov box may be
// at the start or the end of the file, so top-level boxes are skipped by size.
func isoDuration(file io.ReadSeeker, size int64) (float64, error) {
	moovStart, moovSize, err := findBox(file, 0, size, "moov")
	if err != nil {
		return 0, err
	}
	mvhdStart, mvhdSize, err := findBox(file, moovStart, moovStart+moovSize, "mvhd")
	if err != nil {
		return 0, err
	}
	if mvhdSize < 32 {
		return 0, errors.New("mvhd is too short")
	}
	if _, err := file.Seek(mvhdStart, io.SeekStart); err != nil {
		return 0, err
	}
	header := make([]byte, 32)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, err
	}
	var timescale, duration uint64
	if header[0] == 1 {
		timescale = uint64(binary.BigEndian.Uint32(header[20:24]))
		duration = binary.BigEndian.Uint64(header[24:32])
	} else {
		timescale = uint64(binary.BigEndian.Uint32(header[12:16]))
		duration = uint64(binary.BigEndian.Uint32(header[16:20]))
	}
	if timescale == 0 || duration == 0 || duration == math.MaxUint32 || duration == math.MaxUint64 {
		return 0, errors.New("mvhd has no duration")
	}
	return float64(duration) / float64(timescale), nil
}

// findBox returns the payload offset and payload size of the first box of the
// given type between start and end.
func findBox(file io.ReadSeeker, start, end int64, kind string) (int64, int64, error) {
	header := make([]byte, 16)
	for offset := start; offset+8 <= end; {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(file, header[:8]); err != nil {
			return 0, 0, err
		}
		boxSize := int64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		switch boxSize {
		case 0:
			boxSize = end - offset
		case 1:
			if _, err := io.ReadFull(file, header[8:16]); err != nil {
				return 0, 0, err
			}
			boxSize = int64(binary.BigEndian.Uint64(header[8:16]))
			headerSize = 16
		}
		if boxSize < headerSize || offset+boxSize > end {
			return 0, 0, errors.New("malformed box")
		}
		if string(header[4:8]) == kind {
			return offset + headerSize, boxSize - headerSize, nil
		}
		offset += boxSize
	}
	return 0, 0, errors.New("box not found")
}

const (
	ebmlSegment       = 0x18538067
	ebmlInfo          = 0x1549A966
	ebmlCluster       = 0x1F43B675
	ebmlTimecodeScale = 0x2AD7B1
	ebmlDuration      = 0x4489
)

// matroskaDuration reads Segment/Info/Duration, which is stored in timecode
// units (TimecodeScale nanoseconds, one millisecond by default).
func matroskaDuration(file io.ReadSeeker, size int64) (float64, error) {
	reader := &ebmlReader{file: file}
	id, headerSize, err := reader.element(0)
	if err != nil || id != 0x1A45DFA3 {
		return 0, errors.New("not an EBML file")
	}
	offset := reader.offset + headerSize
	for offset < size {
		id, elementSize, err := reader.element(offset)
		if err != nil {
			return 0, err
		}
		if id == ebmlSegment {
			return reader.segmentDuration(reader.offset, min(size, reader.offset+elementSize))
		}
		offset = reader.offset + elementSize
	}
	return 0, errors.New("segment not found")
}

type ebmlReader struct {
	file   io.ReadSeeker
	offset int64 // payload offset of the element read last
}

// element reads the element header at position and returns its ID and payload
// size. An unknown size is reported as the maximum int64.
func (r *ebmlReader) element(position int64) (uint64, int64, error) {
	if _, err := r.file.Seek(position, io.SeekStart); err != nil {
		return 0, 0, err
	}
	id, idLength, err := readVarint(r.file, false)
	if err != nil {
		return 0, 0, err
	}
	payload, sizeLength, err := readVarint(r.file, true)
	if err != nil {
		return 0, 0, err
	}
	r.offset = position + int64(idLength+sizeLength)
	if payload == math.MaxInt64 {
		return id, math.MaxInt64 - r.offset, nil
	}
	return id, int64(payload), nil
}

func (r *ebmlReader) segmentDuration(start, end int64) (float64, error) {
	for offset := start; offset < end; {
		id, size, err := r.element(offset)
		if err != nil {
			return 0, err
		}
		payload := r.offset
		switch id {
		case ebmlInfo:
			return r.infoDuration(payload, min(end, payload+size))
		case ebmlCluster:
			return 0, errors.New("no duration before the first cluster")
		}
		offset = payload + size
	}
	return 0, errors.New("info not found")
}

func (r *ebmlReader) infoDuration(start, end int64) (float64, error) {
	scale, duration := 1_000_000.0, 0.0
	for offset := start; offset < end; {
		id, size, err := r.element(offset)
		if err != nil {
			return 0, err
		}
		payload := r.offset
		if size > 8 && (id == ebmlTimecodeScale || id == ebmlDuration) {
			return 0, errors.New("oversized info field")
		}
		value := make([]byte, max(size, 0))
		if id == ebmlTimecodeScale || id == ebmlDuration {
			if _, err := io.ReadFull(r.file, value); err != nil {
				return 0, err
			}
		}
		switch id {
		case ebmlTimecodeScale:
			var parsed uint64
			for _, part := range value {
				parsed = parsed<<8 | uint64(part)
			}
			if parsed > 0 {
				scale = float64(parsed)
			}
		case ebmlDuration:
			switch size {
			case 4:
				duration = float64(math.Float32frombits(binary.BigEndian.Uint32(value)))
			case 8:
				duration = math.Float64frombits(binary.BigEndian.Uint64(value))
			}
		}
		offset = payload + size
	}
	if duration <= 0 {
		return 0, errors.New("info has no duration")
	}
	return duration * scale / 1e9, nil
}

// readVarint reads an EBML variable-length integer. IDs keep their length
// marker; sizes drop it and report an all-ones value as unknown.
func readVarint(reader io.Reader, isSize bool) (uint64, int, error) {
	var first [1]byte
	if _, err := io.ReadFull(reader, first[:]); err != nil {
		return 0, 0, err
	}
	length := 1
	for mask := byte(0x80); length <= 8 && first[0]&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 {
		return 0, 0, errors.New("invalid EBML varint")
	}
	rest := make([]byte, length-1)
	if _, err := io.ReadFull(reader, rest); err != nil {
		return 0, 0, err
	}
	value := uint64(first[0])
	if isSize {
		value &= uint64(0xFF >> length)
	}
	allOnes := value == uint64(0xFF>>length)
	for _, part := range rest {
		value = value<<8 | uint64(part)
		allOnes = allOnes && part == 0xFF
	}
	if isSize && allOnes {
		return math.MaxInt64, length, nil
	}
	return value, length, nil
}

// ffprobeDuration asks ffprobe, when installed, for any other container.
func ffprobeDuration(path string) float64 {
	executable, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", "--", path).Output()
	if err != nil {
		return 0
	}
	value, err := strconv.ParseFloat(string(bytes.TrimSpace(output)), 64)
	if err != nil || value <= 0 || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0
	}
	return value
}
