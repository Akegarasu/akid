//go:build windows

package logging

import "os"

func truncateRotatingFile(w *RotatingWriter) error {
	// An append-mode Windows handle cannot be truncated in place. Closing and
	// reopening preserves copytruncate's stable path semantics.
	if err := w.file.Close(); err != nil {
		return err
	}
	if err := os.Truncate(w.path, 0); err != nil {
		w.file, _ = os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
		return err
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	w.file = file
	return nil
}
