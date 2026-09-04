//go:build !windows

package logging

func truncateRotatingFile(w *RotatingWriter) error {
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	_, err := w.file.Seek(0, 2)
	return err
}
