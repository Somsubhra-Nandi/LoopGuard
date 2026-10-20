package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Log rotation: daemon.log is rotated to daemon.log.1 (and .1 to .2) when
// it would grow past maxLogSize, so logs never take more than about
// (logBackups+1) × maxLogSize of disk.
const logBackups = 2

var maxLogSize int64 = 5 << 20 // a var for tests

// logFile is an io.Writer appending to a size-capped, rotated log file.
// It is safe for concurrent use.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLogFile(path string) (*logFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &logFile{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > maxLogSize {
		// A failed rotation keeps appending to the current file.
		_ = l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate shifts daemon.log → .1 → .2 (dropping the oldest) and reopens.
// The file is closed first: Windows can't rename an open file.
func (l *logFile) rotate() error {
	l.f.Close()
	for i := logBackups; i >= 1; i-- {
		src := l.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		_ = os.Rename(src, fmt.Sprintf("%s.%d", l.path, i))
	}
	return l.open()
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
