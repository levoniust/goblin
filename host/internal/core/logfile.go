package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LogFile is an append-only log in the data dir that rotates at maxSize: name.log → name.log.1 → … → name.log.<keep>.
// Every node has one (Windows services have no terminal to log to); the local page shows its tail.
type LogFile struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	keep    int
	f       *os.File
	size    int64
}

func OpenLogFile(home, name string) (*LogFile, error) {
	l := &LogFile{path: filepath.Join(home, name), maxSize: 1 << 20, keep: 3}
	return l, l.open()
}

func LogPath(home, name string) string { return filepath.Join(home, name) }

func (l *LogFile) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	l.f, l.size = f, 0
	if st != nil {
		l.size = st.Size()
	}
	return nil
}

func (l *LogFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	if l.size+int64(len(p)) > l.maxSize {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *LogFile) rotate() {
	l.f.Close()
	for i := l.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		l.f = nil
	}
}

// TailFile returns up to max bytes from the end of a file, starting at a line break.
func TailFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(buf, off)
	buf = buf[:n]
	if off > 0 {
		for i, b := range buf {
			if b == '\n' {
				buf = buf[i+1:]
				break
			}
		}
	}
	return string(buf)
}
