// Package diag keeps a small rotating log of what Faro did with rooms,
// players and streams, so a problem such as a slow or interrupted transfer
// can be understood after the fact. Callers must keep tokens, invite
// secrets and local file paths out of it.
package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxLogBytes is when the log rotates; one previous file is kept.
const maxLogBytes = 1 << 20

// Log appends timestamped lines to a file. A nil *Log discards everything.
type Log struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

// Open opens faro.log in dir, creating dir if needed.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log := &Log{path: filepath.Join(dir, "faro.log")}
	if err := log.open(); err != nil {
		return nil, err
	}
	return log, nil
}

// DefaultDir is where the desktop app keeps its log.
func DefaultDir() string {
	if root, err := os.UserCacheDir(); err == nil {
		return filepath.Join(root, "faro", "logs")
	}
	return filepath.Join(os.TempDir(), "faro-logs")
}

func (l *Log) open() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	l.file, l.size = file, info.Size()
	return nil
}

// Printf records one line. Failures to write are ignored: diagnostics must
// never interfere with what they describe.
func (l *Log) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + strings.TrimRight(fmt.Sprintf(format, args...), "\n") + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if l.size+int64(len(line)) > maxLogBytes {
		l.file.Close()
		_ = os.Rename(l.path, l.path+".1")
		if l.open() != nil {
			l.file = nil
			return
		}
	}
	written, _ := l.file.WriteString(line)
	l.size += int64(written)
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
