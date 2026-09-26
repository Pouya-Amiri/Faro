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

// blockStore keeps a bounded part of the file in fixed-size blocks with
// least-recently-used eviction, either in memory (the fallback when no disk
// cache is possible) or in a disk file of a fixed size (when the whole file
// would exceed the configured cache limit).
type blockStore struct {
	mu        sync.Mutex
	blockSize int64
	size      int64
	limit     int
	blocks    map[int64]*list.Element
	order     *list.List

	// Disk-backed stores place blocks in slots of file; memory stores keep
	// each block's bytes in the block itself.
	file      *os.File
	path      string
	freeSlots []int64
	nextSlot  int64
	once      sync.Once
}

type storedBlock struct {
	index int64
	data  []byte // memory stores
	slot  int64  // disk stores
}

func newMemoryStore(size, limitBytes, blockSize int64) *blockStore {
	return &blockStore{
		blockSize: blockSize, size: size, limit: max(2, int(limitBytes/blockSize)),
		blocks: make(map[int64]*list.Element), order: list.New(),
	}
}

// newDiskBlockStore creates a disk cache of limitBytes for a file larger than
// that, removed like newDiskStore's file.
func newDiskBlockStore(dir string, size, limitBytes, blockSize int64) (*blockStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sweepStaleStreamFiles(dir)
	store := newMemoryStore(size, limitBytes, blockSize)
	capacity := int64(store.limit) * blockSize
	if free, err := freeDiskBytes(dir); err != nil || free < uint64(capacity)+diskMargin {
		return nil, errors.New("not enough free disk space for a stream cache")
	}
	file, err := os.CreateTemp(dir, "stream-*.part")
	if err != nil {
		return nil, err
	}
	store.file, store.path = file, file.Name()
	if err := file.Truncate(capacity); err != nil {
		store.Close()
		return nil, err
	}
	if runtime.GOOS != "windows" && os.Remove(file.Name()) == nil {
		store.path = ""
	}
	return store, nil
}

func (s *blockStore) blockLength(index int64) int64 {
	return min(s.blockSize, s.size-index*s.blockSize)
}

// block returns a block, creating it when asked, which may evict the least
// recently used one.
func (s *blockStore) block(index int64, create bool, evicted *[][2]int64) *storedBlock {
	if element, ok := s.blocks[index]; ok {
		s.order.MoveToFront(element)
		return element.Value.(*storedBlock)
	}
	if !create {
		return nil
	}
	block := &storedBlock{index: index}
	if len(s.blocks) >= s.limit {
		oldest := s.order.Back()
		old := oldest.Value.(*storedBlock)
		s.order.Remove(oldest)
		delete(s.blocks, old.index)
		start := old.index * s.blockSize
		*evicted = append(*evicted, [2]int64{start, start + s.blockLength(old.index)})
		s.freeSlots = append(s.freeSlots, old.slot)
	}
	if s.file == nil {
		block.data = make([]byte, s.blockLength(index))
	} else if free := len(s.freeSlots); free > 0 {
		block.slot = s.freeSlots[free-1]
		s.freeSlots = s.freeSlots[:free-1]
	} else {
		block.slot = s.nextSlot
		s.nextSlot++
	}
	s.blocks[index] = s.order.PushFront(block)
	return block
}

func (s *blockStore) ReadAt(data []byte, offset int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read := 0
	for read < len(data) {
		position := offset + int64(read)
		block := s.block(position/s.blockSize, false, nil)
		if block == nil {
			return read, errEvicted
		}
		within := position - block.index*s.blockSize
		length := min(int64(len(data)-read), s.blockLength(block.index)-within)
		if s.file == nil {
			copy(data[read:], block.data[within:within+length])
		} else if _, err := s.file.ReadAt(data[read:read+int(length)], block.slot*s.blockSize+within); err != nil {
			return read, err
		}
		read += int(length)
	}
	return read, nil
}

func (s *blockStore) WriteAt(data []byte, offset int64) ([][2]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evicted [][2]int64
	for written := 0; written < len(data); {
		position := offset + int64(written)
		block := s.block(position/s.blockSize, true, &evicted)
		within := position - block.index*s.blockSize
		length := min(int64(len(data)-written), s.blockLength(block.index)-within)
		if s.file == nil {
			copy(block.data[within:], data[written:written+int(length)])
		} else if _, err := s.file.WriteAt(data[written:written+int(length)], block.slot*s.blockSize+within); err != nil {
			return evicted, err
		}
		written += int(length)
	}
	return evicted, nil
}

func (s *blockStore) Complete() bool { return int64(s.limit)*s.blockSize >= s.size }

// onDisk reports whether a write failure can mean the disk is full.
func (s *blockStore) onDisk() bool { return s.file != nil }

func (s *blockStore) Close() error {
	s.mu.Lock()
	clear(s.blocks)
	s.order.Init()
	s.mu.Unlock()
	var err error
	if s.file != nil {
		s.once.Do(func() {
			err = s.file.Close()
			if s.path != "" {
				_ = os.Remove(s.path)
			}
		})
	}
	return err
}
