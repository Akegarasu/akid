package tui

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"
)

type clipboardCommand struct {
	name   string
	args   []string
	method string
}

func copyToClipboardCmd(text string) tea.Cmd {
	return func() tea.Msg {
		command, ok := findClipboardCommand()
		if !ok {
			return clipboardResultMsg{err: exec.ErrNotFound, text: text}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, command.name, command.args...)
		cmd.Stdin = bytes.NewBufferString(text)
		err := cmd.Run()
		return clipboardResultMsg{method: command.method, err: err, text: text}
	}
}

func writeSelectionCmd(text string) tea.Cmd {
	return func() tea.Msg {
		file, err := os.CreateTemp("", "akid-copy-*.txt")
		if err != nil {
			return selectionWrittenMsg{err: err}
		}
		path := file.Name()
		if _, err = file.WriteString(text); err == nil {
			err = file.Close()
		} else {
			_ = file.Close()
		}
		if err != nil {
			_ = os.Remove(path)
			return selectionWrittenMsg{err: err}
		}
		return selectionWrittenMsg{path: path}
	}
}

func findClipboardCommand() (clipboardCommand, bool) {
	var candidates []clipboardCommand
	switch runtime.GOOS {
	case "windows":
		candidates = []clipboardCommand{{name: "clip.exe", method: "Windows clipboard"}}
	case "darwin":
		candidates = []clipboardCommand{{name: "pbcopy", method: "pbcopy"}}
	default:
		candidates = []clipboardCommand{
			{name: "wl-copy", method: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}, method: "xclip"},
			{name: "xsel", args: []string{"--clipboard", "--input"}, method: "xsel"},
		}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate.name); err == nil {
			candidate.name = path
			return candidate, true
		}
	}
	return clipboardCommand{}, false
}
