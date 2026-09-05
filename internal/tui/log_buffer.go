package tui

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	akidlog "akid/internal/logging"
)

const (
	MaxLogLines = 10_000
	MaxLogBytes = 2 << 20
	LogPageSize = 256 << 10
)

type selectionMode uint8

const (
	selectionNone selectionMode = iota
	selectionCharacter
	selectionLine
)

type logPosition struct {
	Line int
	Col  int
}

type LogLine struct {
	Text        string
	StartOffset int64
	EndOffset   int64
	bytes       int
	raw         []byte
	partial     bool
}

// LogBuffer is the framework-independent state of the log viewer. Offsets are
// byte offsets in the current log generation; cursor and selection positions
// are indexes into the bounded in-memory line window.
type LogBuffer struct {
	Lines       []LogLine
	StartOffset int64
	EndOffset   int64
	Generation  uint64
	CursorLine  int
	CursorCol   int
	ViewTop     int
	Horizontal  int
	Follow      bool
	EOF         bool

	loadedBytes int
	selection   selectionMode
	anchor      logPosition
}

func NewLogBuffer() LogBuffer {
	return LogBuffer{Follow: true, EOF: true}
}

func (b *LogBuffer) Reset(chunk akidlog.LogChunk, follow bool) {
	b.reset(chunk, follow, !follow)
}

func (b *LogBuffer) reset(chunk akidlog.LogChunk, follow, keepHead bool) {
	b.Lines = parseLogLines(chunk)
	b.Generation = chunk.Generation
	b.Follow = follow
	b.EOF = chunk.EOF
	b.Horizontal = 0
	b.selection = selectionNone
	b.recount()
	b.trim(keepHead)
	if len(b.Lines) == 0 {
		b.StartOffset = chunk.StartOffset
		b.EndOffset = chunk.EndOffset
		b.CursorLine, b.CursorCol, b.ViewTop = 0, 0, 0
		return
	}
	b.syncOffsets()
	if follow {
		b.CursorLine = len(b.Lines) - 1
		b.CursorCol = runeCount(b.Lines[b.CursorLine].Text)
	} else {
		b.CursorLine, b.CursorCol = 0, 0
	}
	b.ViewTop = 0
}

func (b *LogBuffer) Prepend(chunk akidlog.LogChunk) bool {
	if chunk.Generation != b.Generation {
		b.Reset(chunk, b.Follow)
		return false
	}
	parsed := parseLogLines(chunk)
	if len(b.Lines) > 0 {
		kept := parsed[:0]
		for _, line := range parsed {
			if line.EndOffset <= b.StartOffset {
				kept = append(kept, line)
			}
		}
		parsed = kept
	}
	if len(parsed) == 0 {
		return true
	}
	if len(b.Lines) > 0 {
		last := len(parsed) - 1
		if parsed[last].partial && parsed[last].EndOffset == b.Lines[0].StartOffset {
			prefixRunes := runeCount(parsed[last].Text)
			parsed[last] = mergeLogLines(parsed[last], b.Lines[0])
			b.Lines = b.Lines[1:]
			if b.CursorLine == 0 {
				b.CursorCol += prefixRunes
			}
			b.CursorLine--
			b.ViewTop--
			if b.selection != selectionNone {
				b.CancelSelection()
			}
		}
	}
	shift := len(parsed)
	b.Lines = append(parsed, b.Lines...)
	b.CursorLine += shift
	b.ViewTop += shift
	if b.selection != selectionNone {
		b.anchor.Line += shift
	}
	b.recount()
	b.trim(true)
	b.syncOffsets()
	return true
}

func (b *LogBuffer) Append(chunk akidlog.LogChunk) bool {
	if chunk.Generation != b.Generation {
		return false
	}
	parsed := parseLogLines(chunk)
	if len(b.Lines) > 0 {
		kept := parsed[:0]
		for _, line := range parsed {
			if line.StartOffset >= b.EndOffset {
				kept = append(kept, line)
			}
		}
		parsed = kept
	}
	if len(parsed) == 0 {
		b.EndOffset = max(b.EndOffset, chunk.EndOffset)
		b.EOF = chunk.EOF
		return true
	}
	if len(b.Lines) > 0 {
		last := len(b.Lines) - 1
		if b.Lines[last].partial && b.Lines[last].EndOffset == parsed[0].StartOffset {
			parsed[0] = mergeLogLines(b.Lines[last], parsed[0])
			b.Lines = b.Lines[:last]
		}
	}
	b.Lines = append(b.Lines, parsed...)
	b.recount()
	b.EOF = chunk.EOF
	b.trim(false)
	if len(b.Lines) == 0 {
		b.StartOffset, b.EndOffset = chunk.EndOffset, chunk.EndOffset
	}
	b.syncOffsets()
	if b.Follow && len(b.Lines) > 0 {
		b.CursorLine = len(b.Lines) - 1
		b.CursorCol = runeCount(b.Lines[b.CursorLine].Text)
	}
	return true
}

func (b *LogBuffer) MoveLines(delta, height int) {
	if len(b.Lines) == 0 {
		return
	}
	b.CursorLine = clamp(b.CursorLine+delta, 0, len(b.Lines)-1)
	b.CursorCol = min(b.CursorCol, runeCount(b.Lines[b.CursorLine].Text))
	b.ensureVisible(height)
}

func (b *LogBuffer) MoveColumn(delta int) {
	if len(b.Lines) == 0 {
		return
	}
	for delta != 0 {
		direction := 1
		if delta < 0 {
			direction = -1
		}
		b.CursorCol = moveTextColumn(b.Lines[b.CursorLine].Text, b.CursorCol, direction)
		delta -= direction
	}
}

func (b *LogBuffer) EnsureCursorHorizontal(width int) {
	if width < 1 {
		width = 1
	}
	if len(b.Lines) == 0 {
		return
	}
	text := b.Lines[b.CursorLine].Text
	column := textCellColumn(text, b.CursorCol)
	end := max(column+1, textCellColumn(text, moveTextColumn(text, b.CursorCol, 1)))
	if column < b.Horizontal {
		b.Horizontal = column
	}
	if end > b.Horizontal+width {
		b.Horizontal = end - width
	}
}

func (b *LogBuffer) Top() {
	b.CursorLine, b.CursorCol, b.ViewTop = 0, 0, 0
	b.Follow = false
}

func (b *LogBuffer) Bottom(height int) {
	if len(b.Lines) == 0 {
		return
	}
	b.CursorLine = len(b.Lines) - 1
	b.CursorCol = runeCount(b.Lines[b.CursorLine].Text)
	b.ensureVisible(height)
}

func (b *LogBuffer) ToggleSelection(mode selectionMode) {
	if b.selection == mode {
		b.selection = selectionNone
		return
	}
	b.selection = mode
	b.anchor = logPosition{Line: b.CursorLine, Col: b.CursorCol}
}

func (b *LogBuffer) CancelSelection() { b.selection = selectionNone }

func (b *LogBuffer) SelectionMode() selectionMode { return b.selection }

func (b *LogBuffer) SelectedText() string {
	if b.selection == selectionNone || len(b.Lines) == 0 {
		return ""
	}
	start, end := b.orderedSelection()
	if b.selection == selectionLine {
		parts := make([]string, 0, end.Line-start.Line+1)
		for i := start.Line; i <= end.Line; i++ {
			parts = append(parts, b.Lines[i].Text)
		}
		return strings.Join(parts, "\n") + "\n"
	}

	parts := make([]string, 0, end.Line-start.Line+1)
	for i := start.Line; i <= end.Line; i++ {
		runes := []rune(b.Lines[i].Text)
		from, to := 0, len(runes)
		if i == start.Line {
			from = clamp(start.Col, 0, len(runes))
		}
		if i == end.Line {
			to = moveTextColumn(b.Lines[i].Text, end.Col, 1)
		}
		if from > to {
			from = to
		}
		parts = append(parts, string(runes[from:to]))
	}
	return strings.Join(parts, "\n")
}

// SelectionRange returns the selected rune interval [start, end) for a line.
func (b *LogBuffer) SelectionRange(line int) (start, end int, selected bool) {
	if b.selection == selectionNone || line < 0 || line >= len(b.Lines) {
		return 0, 0, false
	}
	first, last := b.orderedSelection()
	if line < first.Line || line > last.Line {
		return 0, 0, false
	}
	length := runeCount(b.Lines[line].Text)
	if b.selection == selectionLine {
		return 0, length, true
	}
	start, end = 0, length
	if line == first.Line {
		start = clamp(first.Col, 0, length)
	}
	if line == last.Line {
		end = moveTextColumn(b.Lines[line].Text, last.Col, 1)
	}
	return start, end, true
}

func (b *LogBuffer) Find(query string, direction int, height int) bool {
	query = strings.ToLower(query)
	if query == "" || len(b.Lines) == 0 {
		return false
	}
	for step := 1; step <= len(b.Lines); step++ {
		i := (b.CursorLine + direction*step) % len(b.Lines)
		if i < 0 {
			i += len(b.Lines)
		}
		if strings.Contains(strings.ToLower(b.Lines[i].Text), query) {
			b.CursorLine = i
			b.CursorCol = 0
			b.Follow = false
			b.ensureVisible(height)
			return true
		}
	}
	return false
}

func (b *LogBuffer) ensureVisible(height int) {
	if height < 1 {
		height = 1
	}
	if b.CursorLine < b.ViewTop {
		b.ViewTop = b.CursorLine
	}
	if b.CursorLine >= b.ViewTop+height {
		b.ViewTop = b.CursorLine - height + 1
	}
	maxTop := max(0, len(b.Lines)-height)
	b.ViewTop = clamp(b.ViewTop, 0, maxTop)
}

func (b *LogBuffer) orderedSelection() (logPosition, logPosition) {
	cursor := logPosition{Line: b.CursorLine, Col: b.CursorCol}
	anchor := b.anchor
	if anchor.Line < cursor.Line || (anchor.Line == cursor.Line && anchor.Col <= cursor.Col) {
		return anchor, cursor
	}
	return cursor, anchor
}

func (b *LogBuffer) trim(fromEnd bool) {
	droppedEnd := false
	for len(b.Lines) > MaxLogLines || b.loadedBytes > MaxLogBytes {
		if len(b.Lines) == 0 {
			break
		}
		if len(b.Lines) == 1 && b.Lines[0].bytes > MaxLogBytes {
			line := b.Lines[0]
			// Keep a bounded fragment of a line larger than the whole window.
			// Account for both the raw bytes and the sanitized display text.
			drop := max(1, len(line.raw)/2)
			if fromEnd {
				line.raw = line.raw[:len(line.raw)-drop]
				line.EndOffset -= int64(drop)
				droppedEnd = true
			} else {
				line.raw = line.raw[drop:]
				line.StartOffset += int64(drop)
			}
			b.Lines = parseLogLines(akidlog.LogChunk{StartOffset: line.StartOffset, Data: line.raw})
			b.CancelSelection()
			b.recount()
			continue
		}
		if fromEnd {
			last := len(b.Lines) - 1
			b.loadedBytes -= b.Lines[last].bytes
			b.Lines = b.Lines[:last]
			droppedEnd = true
			continue
		}
		b.loadedBytes -= b.Lines[0].bytes
		b.Lines = b.Lines[1:]
		b.CursorLine--
		b.ViewTop--
		if b.selection != selectionNone {
			b.anchor.Line--
			if b.anchor.Line < 0 {
				b.selection = selectionNone
			}
		}
	}
	if droppedEnd {
		b.EOF = false
	}
	if len(b.Lines) == 0 {
		b.CursorLine, b.CursorCol, b.ViewTop = 0, 0, 0
		b.selection = selectionNone
		return
	}
	if b.selection != selectionNone && (b.anchor.Line < 0 || b.anchor.Line >= len(b.Lines)) {
		b.selection = selectionNone
	}
	b.CursorLine = clamp(b.CursorLine, 0, len(b.Lines)-1)
	b.ViewTop = clamp(b.ViewTop, 0, len(b.Lines)-1)
}

func (b *LogBuffer) recount() {
	b.loadedBytes = 0
	for _, line := range b.Lines {
		b.loadedBytes += line.bytes
	}
}

func (b *LogBuffer) syncOffsets() {
	if len(b.Lines) == 0 {
		return
	}
	b.StartOffset = b.Lines[0].StartOffset
	b.EndOffset = b.Lines[len(b.Lines)-1].EndOffset
}

func parseLogLines(chunk akidlog.LogChunk) []LogLine {
	if len(chunk.Data) == 0 {
		return nil
	}
	lines := make([]LogLine, 0, bytes.Count(chunk.Data, []byte{'\n'})+1)
	offset := chunk.StartOffset
	for len(chunk.Data) > 0 {
		i := bytes.IndexByte(chunk.Data, '\n')
		consumed := len(chunk.Data)
		raw := chunk.Data
		if i >= 0 {
			consumed = i + 1
			raw = chunk.Data[:i]
		}
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
		text := sanitizeLogText(raw)
		lines = append(lines, LogLine{
			Text:        text,
			StartOffset: offset,
			EndOffset:   offset + int64(consumed),
			bytes:       consumed + len(text),
			raw:         bytes.Clone(chunk.Data[:consumed]),
			partial:     i < 0,
		})
		offset += int64(consumed)
		chunk.Data = chunk.Data[consumed:]
	}
	return lines
}

func mergeLogLines(first, second LogLine) LogLine {
	data := make([]byte, 0, len(first.raw)+len(second.raw))
	data = append(data, first.raw...)
	data = append(data, second.raw...)
	return parseLogLines(akidlog.LogChunk{StartOffset: first.StartOffset, Data: data})[0]
}

func sanitizeLogText(raw []byte) string {
	text := strings.ToValidUTF8(string(raw), "�")
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\t':
			out.WriteString("    ")
		case r == 0x1b:
			out.WriteRune('␛')
		case unicode.IsControl(r):
			out.WriteRune('�')
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func runeCount(s string) int { return utf8.RuneCountInString(s) }

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
