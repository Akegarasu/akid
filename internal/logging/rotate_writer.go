package logging

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// RotatingWriter provides the daemon's own low-volume diagnostic log with the
// same size/retention policy as managed process logs. It uses copytruncate so
// any inherited diagnostic descriptor keeps pointing at the active file.
type RotatingWriter struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	maxSize int64
	keep    int
}

func NewRotatingWriter(path string, maxSize int64, keep int) (*RotatingWriter, error) {
	_ = os.Remove(path + ".rotate.tmp")
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if keep < 0 {
		keep = DefaultKeep
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	return &RotatingWriter{path: path, file: file, maxSize: maxSize, keep: keep}, nil
}

func (w *RotatingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() > 0 && info.Size()+int64(len(data)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	return w.file.Write(data)
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingWriter) rotateLocked() error {
	if w.keep > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
		for i := w.keep - 1; i >= 1; i-- {
			oldPath := fmt.Sprintf("%s.%d", w.path, i)
			newPath := fmt.Sprintf("%s.%d", w.path, i+1)
			if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		tmp := w.path + ".rotate.tmp"
		if err := copyFile(w.path, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, w.path+".1"); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return truncateRotatingFile(w)
}
