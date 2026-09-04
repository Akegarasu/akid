//go:build linux

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type instanceLock struct{ file *os.File }

func acquireLock(path string) (*instanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("akid daemon is already running")
		}
		return nil, err
	}
	return &instanceLock{file: file}, nil
}

func (l *instanceLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}
