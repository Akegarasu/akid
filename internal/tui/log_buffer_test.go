package tui

import (
	"fmt"
	"strings"
	"testing"

	akidlog "akid/internal/logging"
)

func TestLogBufferResetParsesOffsetsAndSanitizesControls(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(akidlog.LogChunk{
		StartOffset: 10,
		EndOffset:   27,
		Generation:  3,
		Data:        []byte("one\r\ntwo\t\x1b[2J\n"),
		EOF:         true,
	}, true)

	if len(buffer.Lines) != 2 {
		t.Fatalf("got %d lines", len(buffer.Lines))
	}
	if buffer.Lines[0].Text != "one" || buffer.Lines[0].StartOffset != 10 || buffer.Lines[0].EndOffset != 15 {
		t.Fatalf("unexpected first line: %#v", buffer.Lines[0])
	}
	if got := buffer.Lines[1].Text; got != "two    ␛[2J" {
		t.Fatalf("control characters were not sanitized: %q", got)
	}
	if buffer.Generation != 3 || buffer.CursorLine != 1 || !buffer.Follow {
		t.Fatalf("unexpected state: %#v", buffer)
	}
}

func TestLogBufferPrependPreservesCursorAndDeduplicatesOverlap(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(chunk(4, 1, "c\nd\n"), false)
	buffer.CursorLine = 1
	buffer.ToggleSelection(selectionLine)

	if ok := buffer.Prepend(chunk(0, 1, "a\nb\nc\n")); !ok {
		t.Fatal("prepend unexpectedly reported a generation change")
	}
	if got := lineTexts(buffer.Lines); got != "a|b|c|d" {
		t.Fatalf("lines = %q", got)
	}
	if buffer.CursorLine != 3 || buffer.anchor.Line != 3 {
		t.Fatalf("cursor/anchor did not move with prepended lines: cursor=%d anchor=%d", buffer.CursorLine, buffer.anchor.Line)
	}
}

func TestLogBufferAppendGenerationAndFollow(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(chunk(0, 8, "one\n"), true)
	if buffer.Append(chunk(4, 9, "two\n")) {
		t.Fatal("append accepted a different generation")
	}
	if !buffer.Append(chunk(4, 8, "two\n")) {
		t.Fatal("append rejected matching generation")
	}
	if buffer.CursorLine != 1 || buffer.EndOffset != 8 {
		t.Fatalf("follow did not move to end: cursor=%d end=%d", buffer.CursorLine, buffer.EndOffset)
	}
}

func TestLogBufferSelection(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(chunk(0, 1, "alpha\nbeta\ngamma\n"), false)
	buffer.CursorLine = 0
	buffer.ToggleSelection(selectionLine)
	buffer.MoveLines(1, 10)
	if got := buffer.SelectedText(); got != "alpha\nbeta\n" {
		t.Fatalf("line selection = %q", got)
	}

	buffer.CancelSelection()
	buffer.CursorLine, buffer.CursorCol = 0, 2
	buffer.ToggleSelection(selectionCharacter)
	buffer.CursorLine, buffer.CursorCol = 1, 1
	if got := buffer.SelectedText(); got != "pha\nbe" {
		t.Fatalf("character selection = %q", got)
	}
}

func TestLogBufferKeepsCharacterSelectionCursorVisible(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(chunk(0, 1, "0123456789\n"), false)
	buffer.ToggleSelection(selectionCharacter)
	buffer.MoveColumn(8)
	buffer.EnsureCursorHorizontal(5)
	if buffer.Horizontal != 4 {
		t.Fatalf("horizontal offset = %d", buffer.Horizontal)
	}
	buffer.MoveColumn(-7)
	buffer.EnsureCursorHorizontal(5)
	if buffer.Horizontal != 1 {
		t.Fatalf("horizontal offset did not move left: %d", buffer.Horizontal)
	}
}

func TestLogBufferSearchWrapsAndDisablesFollow(t *testing.T) {
	buffer := NewLogBuffer()
	buffer.Reset(chunk(0, 1, "first timeout\nsecond\nthird timeout\n"), true)
	if !buffer.Find("TIMEOUT", 1, 2) || buffer.CursorLine != 0 {
		t.Fatalf("forward wrapped search cursor=%d", buffer.CursorLine)
	}
	if buffer.Follow {
		t.Fatal("search should disable follow")
	}
	if !buffer.Find("timeout", -1, 2) || buffer.CursorLine != 2 {
		t.Fatalf("reverse wrapped search cursor=%d", buffer.CursorLine)
	}
}

func TestLogBufferBoundsLineCount(t *testing.T) {
	var data strings.Builder
	for i := 0; i < MaxLogLines+250; i++ {
		fmt.Fprintf(&data, "%05d\n", i)
	}
	buffer := NewLogBuffer()
	buffer.Reset(chunk(0, 1, data.String()), true)
	if len(buffer.Lines) != MaxLogLines {
		t.Fatalf("line cap not enforced: %d", len(buffer.Lines))
	}
	if buffer.Lines[0].Text != "00250" {
		t.Fatalf("wrong side trimmed: first=%q", buffer.Lines[0].Text)
	}
}

func chunk(offset int64, generation uint64, data string) akidlog.LogChunk {
	return akidlog.LogChunk{
		StartOffset: offset,
		EndOffset:   offset + int64(len(data)),
		Generation:  generation,
		Data:        []byte(data),
		EOF:         true,
	}
}

func lineTexts(lines []LogLine) string {
	values := make([]string, len(lines))
	for i := range lines {
		values[i] = lines[i].Text
	}
	return strings.Join(values, "|")
}
