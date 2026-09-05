package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"akid/internal/model"
)

type LogViewer struct {
	Process     model.ProcessInfo
	Stream      model.LogStream
	Buffer      LogBuffer
	SearchInput textinput.Model
	Searching   bool
	Query       string
	Loading     bool
	Unseen      int
}

func NewLogViewer() LogViewer {
	search := textinput.New()
	search.Prompt = "/ "
	search.CharLimit = 256
	search.SetWidth(40)
	return LogViewer{Stream: model.LogStdout, Buffer: NewLogBuffer(), SearchInput: search}
}

func (v *LogViewer) Reset(info model.ProcessInfo, stream model.LogStream) {
	v.Process = info
	v.Stream = stream
	v.Buffer = NewLogBuffer()
	v.Searching = false
	v.Query = ""
	v.SearchInput.SetValue("")
	v.SearchInput.Blur()
	v.Loading = true
	v.Unseen = 0
}

func (v *LogViewer) BeginSearch() {
	v.Searching = true
	v.SearchInput.SetValue(v.Query)
	v.SearchInput.SetCursor(len([]rune(v.Query)))
	v.SearchInput.Focus()
}

func (v *LogViewer) EndSearch(commit bool, contentHeight int) {
	if commit {
		v.Query = strings.TrimSpace(v.SearchInput.Value())
		if v.Query != "" {
			v.Buffer.Find(v.Query, 1, contentHeight)
		}
	}
	v.Searching = false
	v.SearchInput.Blur()
}

func (v *LogViewer) Render(width, height int, status string, statusError bool) string {
	if width < 1 {
		width = 1
	}
	if height < 4 {
		height = 4
	}
	bodyHeight := max(1, height-3)
	header := renderHeader(fmt.Sprintf("%s / %s", v.Process.Config.Name, v.Stream), width)
	body := v.renderBody(width, bodyHeight)
	info := v.renderStatus(width, status, statusError)
	help := renderFooter(v.helpText(), width)
	return strings.Join([]string{header, body, info, help}, "\n")
}

func (v *LogViewer) renderBody(width, height int) string {
	lines := make([]string, 0, height)
	if v.Loading && len(v.Buffer.Lines) == 0 {
		lines = append(lines, subtleStyle.Render("Loading logs…"))
	} else if len(v.Buffer.Lines) == 0 {
		lines = append(lines, subtleStyle.Render("No log output"))
	} else {
		end := min(len(v.Buffer.Lines), v.Buffer.ViewTop+height)
		for i := v.Buffer.ViewTop; i < end; i++ {
			lines = append(lines, v.renderLine(i, width))
		}
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (v *LogViewer) renderLine(index, width int) string {
	line := v.Buffer.Lines[index].Text
	runes := []rune(line)
	visible := runes

	start, end, selected := v.Buffer.SelectionRange(index)
	var rendered string
	if selected {
		start = clamp(start, 0, len(visible))
		end = clamp(end, 0, len(visible))
		if start > end {
			start = end
		}
		if start == end && v.Buffer.SelectionMode() == selectionLine {
			rendered = selectionStyle.Render(string(visible))
		} else {
			rendered = string(visible[:start]) + selectionStyle.Render(string(visible[start:end])) + string(visible[end:])
		}
	} else {
		rendered = string(visible)
		if v.Query != "" && strings.Contains(strings.ToLower(line), strings.ToLower(v.Query)) {
			rendered = matchStyle.Render(rendered)
		}
	}
	if index == v.Buffer.CursorLine && v.Buffer.SelectionMode() == selectionNone {
		rendered = cursorLineStyle.Render(rendered)
	}
	return lipgloss.NewStyle().Inline(true).MaxWidth(width).Render(ansi.Cut(rendered, v.Buffer.Horizontal, v.Buffer.Horizontal+width))
}

func (v *LogViewer) renderStatus(width int, message string, isError bool) string {
	var content string
	if v.Searching {
		v.SearchInput.SetWidth(max(1, width-2))
		content = v.SearchInput.View()
	} else if message != "" {
		style := successStyle
		if isError {
			style = errorStyle
		}
		content = style.Render(message)
	} else {
		mode := "BROWSE"
		if v.Buffer.Follow {
			mode = "FOLLOW"
		}
		selection := ""
		switch v.Buffer.SelectionMode() {
		case selectionCharacter:
			selection = " │ CHAR SELECT"
		case selectionLine:
			selection = " │ LINE SELECT"
		}
		search := ""
		if v.Query != "" {
			search = " │ /" + v.Query
		}
		unseen := ""
		if v.Unseen > 0 {
			unseen = fmt.Sprintf(" │ %d new", v.Unseen)
		}
		content = fmt.Sprintf(" %s │ %s │ line %d/%d │ %s%s%s%s",
			mode, v.Stream, displayLine(v.Buffer.CursorLine, len(v.Buffer.Lines)), len(v.Buffer.Lines),
			formatBytes(v.Buffer.EndOffset-v.Buffer.StartOffset), selection, search, unseen)
	}
	return statusStyle.Width(width).MaxWidth(width).Render(content)
}

func (v *LogViewer) helpText() string {
	if v.Searching {
		return "Enter find  Esc cancel"
	}
	if v.Buffer.SelectionMode() != selectionNone {
		return "↑↓←→ extend  y copy  w write file  Esc cancel"
	}
	return "↑↓ scroll  PgUp/PgDn page  g/G top/bottom  f follow  / find  v/V select  y copy  w file  1/2 stream  Esc back"
}

func displayLine(cursor, count int) int {
	if count == 0 {
		return 0
	}
	return cursor + 1
}
