// Package logx is a size-rotated log file (agent.log → agent.log.1) used in
// headless/service mode.
package logx

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// DefaultMaxBytes rotates the log at ~5 MB.
const DefaultMaxBytes = 5 << 20

// File is an append-only log file that rotates itself. Safe for concurrent use.
type File struct {
	Path     string
	MaxBytes int64
	// Echo, when set, also receives every line (e.g. stdout when run from a console).
	Echo func(line string)

	mu   sync.Mutex
	f    *os.File
	size int64
}

func Open(path string, maxBytes int64) (*File, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	l := &File{Path: path, MaxBytes: maxBytes}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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

func (l *File) rotate() error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	old := l.Path + ".1"
	_ = os.Remove(old)
	if err := os.Rename(l.Path, old); err != nil && !os.IsNotExist(err) {
		// Could not rotate (file locked by a viewer): truncate instead.
		_ = os.Truncate(l.Path, 0)
	}
	return l.open()
}

// Printf writes one timestamped line.
func (l *File) Printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	stamped := time.Now().Format("2006-01-02 15:04:05") + " " + line + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Echo != nil {
		l.Echo(line)
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return
		}
	}
	if l.size+int64(len(stamped)) > l.MaxBytes && l.size > 0 {
		if err := l.rotate(); err != nil {
			return
		}
	}
	n, _ := l.f.WriteString(stamped)
	l.size += int64(n)
}

func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
