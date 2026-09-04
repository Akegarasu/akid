//go:build !linux

package daemon

import (
	"errors"
	"os"
)

type instanceLock struct {
	file *os.File
	path string
}

func acquireLock(path string) (*instanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, errors.New("akid daemon lock exists; Linux is required for reliable daemon locking")
	}
	if err != nil {
		return nil, err
	}
	return &instanceLock{file: file, path: path}, nil
}
func (l *instanceLock) Close() error {
	err := l.file.Close()
	_ = os.Remove(l.path)
	return err
}
