package tui

import (
	"os"
	"runtime"
	"testing"
)

func TestWriteSelectionCmd(t *testing.T) {
	message := writeSelectionCmd("selected\ntext\n")().(selectionWrittenMsg)
	if message.err != nil {
		t.Fatal(message.err)
	}
	defer os.Remove(message.path)
	data, err := os.ReadFile(message.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "selected\ntext\n" {
		t.Fatalf("written selection = %q", data)
	}
	info, err := os.Stat(message.path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("selection file permissions are too broad: %o", info.Mode().Perm())
	}
}
