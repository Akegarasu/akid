//go:build windows

package storage

import "os"

func replaceFile(from, to string) error {
	_ = os.Remove(to)
	return os.Rename(from, to)
}

// Windows does not expose a useful directory fsync through os.File.
func syncDirectory(string) error { return nil }
