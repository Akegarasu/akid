//go:build !windows

package storage

import "os"

func replaceFile(from, to string) error {
	return os.Rename(from, to)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
