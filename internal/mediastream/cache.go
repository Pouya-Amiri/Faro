package mediastream

import (
	"container/list"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// cacheStore holds the streamed file's bytes on the viewer. The gateway
// tracks which ranges are present; the store only moves bytes.
type cacheStore interface {
	io.ReaderAt
	// WriteAt stores data and reports ranges the store dropped to make room.
	WriteAt(data []byte, offset int64) (evicted [][2]int64, err error)
	// Complete reports whether the store can hold the whole file, which is
	// what makes downloading the rest of it in the background worthwhile.
	Complete() bool
	Close() error
}

var errEvicted = errors.New("cached range was evicted")

// diskMargin keeps a disk cache from filling the volume.
const diskMargin = 2 << 30

// newDiskStore creates a sparse file as large as the media in dir. On Unix the
// name is removed as soon as the file is open, so nothing is left behind even
// if Faro crashes; on Windows it is removed on Close and stale files from an
// earlier crash are swept away.
func newDiskStore(dir string, size int64) (cacheStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sweepStaleStreamFiles(dir)
	if free, err := freeDiskBytes(dir); err != nil || free < uint64(size)+diskMargin {
		return nil, errors.New("not enough free disk space for a stream cache")
	}
	file, err := os.CreateTemp(dir, "stream-*.part")
	if err != nil {
		return nil, err
	}
	store := &diskStore{file: file, path: file.Name()}
	if err := file.Truncate(size); err != nil {
		store.Close()
		return nil, err
	}
	if runtime.GOOS != "windows" && os.Remove(file.Name()) == nil {
		store.path = ""
	}
	return store, nil
}

type diskStore struct {
	file *os.File
	path string
	once sync.Once
}

func (s *diskStore) ReadAt(data []byte, offset int64) (int, error) {
	return s.file.ReadAt(data, offset)
}

func (s *diskStore) WriteAt(data []byte, offset int64) ([][2]int64, error) {
	_, err := s.file.WriteAt(data, offset)
	return nil, err
}

func (s *diskStore) Complete() bool { return true }

func (s *diskStore) Close() error {
	var err error
	s.once.Do(func() {
		err = s.file.Close()
		if s.path != "" {
			_ = os.Remove(s.path)
		}
	})
	return err
}

func sweepStaleStreamFiles(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "stream-*.part"))
	for _, match := range matches {
		if info, err := os.Stat(match); err == nil && time.Since(info.ModTime()) > 12*time.Hour {
			_ = os.Remove(match)
		}
	}
}

// memoryStore is the fallback when no disk cache is possible: fixed-size
// blocks with least-recently-used eviction.
type memoryStore struct {
	mu        sync.Mutex
	blockSize int64
	size      int64
	limit     int
	blocks    map[int64]*list.Element
	order     *list.List
}

type memoryBlock struct {
	index int64
	data  []byte
}

func newMemoryStore(size, limitBytes, blockSize int64) *memoryStore {
	return &memoryStore{
		blockSize: blockSize, size: size, limit: max(1, int(limitBytes/blockSize)),
		blocks: make(map[int64]*list.Element), order: list.New(),
	}
}

func (s *memoryStore) block(index int64, create bool) *memoryBlock {
	if element, ok := s.blocks[index]; ok {
		s.order.MoveToFront(element)
		return element.Value.(*memoryBlock)
	}
	if !create {
		return nil
	}
	length := min(s.blockSize, s.size-index*s.blockSize)
	block := &memoryBlock{index: index, data: make([]byte, length)}
	s.blocks[index] = s.order.PushFront(block)
	return block
}

func (s *memoryStore) ReadAt(data []byte, offset int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read := 0
	for read < len(data) {
		position := offset + int64(read)
		block := s.block(position/s.blockSize, false)
		if block == nil {
			return read, errEvicted
		}
		read += copy(data[read:], block.data[position-block.index*s.blockSize:])
	}
	return read, nil
}

func (s *memoryStore) WriteAt(data []byte, offset int64) ([][2]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for written := 0; written < len(data); {
		position := offset + int64(written)
		block := s.block(position/s.blockSize, true)
		written += copy(block.data[position-block.index*s.blockSize:], data[written:])
	}
	var evicted [][2]int64
	for len(s.blocks) > s.limit {
		oldest := s.order.Back()
		block := oldest.Value.(*memoryBlock)
		s.order.Remove(oldest)
		delete(s.blocks, block.index)
		start := block.index * s.blockSize
		evicted = append(evicted, [2]int64{start, start + int64(len(block.data))})
	}
	return evicted, nil
}

func (s *memoryStore) Complete() bool { return int64(s.limit)*s.blockSize >= s.size }

func (s *memoryStore) Close() error {
	s.mu.Lock()
	clear(s.blocks)
	s.order.Init()
	s.mu.Unlock()
	return nil
}
