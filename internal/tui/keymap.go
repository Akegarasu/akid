package tui

import (
	tea "charm.land/bubbletea/v2"

	"akid/internal/model"
)

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		m.closeLogSubscription()
		return m, tea.Quit
	}
	if m.view == viewProcesses && m.processes.searching {
		switch key {
		case "enter":
			m.processes.EndSearch(true)
			return m, nil
		case "esc":
			m.processes.EndSearch(false)
			return m, nil
		}
		var cmd tea.Cmd
		m.processes.searchInput, cmd = m.processes.searchInput.Update(msg)
		m.processes.UpdateSearch(m.processes.searchInput.Value())
		return m, cmd
	}
	if m.view == viewLogs && m.logs.Searching {
		switch key {
		case "enter":
			m.logs.EndSearch(true, m.logBodyHeight())
			return m, nil
		case "esc":
			m.logs.EndSearch(false, m.logBodyHeight())
			return m, nil
		}
		var cmd tea.Cmd
		m.logs.SearchInput, cmd = m.logs.SearchInput.Update(msg)
		return m, cmd
	}

	switch m.view {
	case viewProcessDetail:
		return m.handleDetailKey(key)
	case viewLogs:
		return m.handleLogKey(key)
	default:
		return m.handleProcessKey(key)
	}
}

func (m Model) handleProcessKey(key string) (tea.Model, tea.Cmd) {
	height := max(1, m.height-4)
	switch key {
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.processes.Move(-1, height)
	case "down", "j":
		m.processes.Move(1, height)
	case "pgup":
		m.processes.Move(-height, height)
	case "pgdown":
		m.processes.Move(height, height)
	case "home", "g":
		m.processes.Home(height)
	case "end", "G":
		m.processes.End(height)
	case "/":
		m.processes.BeginSearch()
	case "enter", "l":
		if info, ok := m.processes.Selected(); ok {
			return m.openLogs(info, model.LogStdout)
		}
	case "i", "tab":
		if info, ok := m.processes.Selected(); ok {
			m.detail = info
			m.detailTop = 0
			m.view = viewProcessDetail
		}
	case "a", "r", "s":
		return m.actionForSelected(key)
	}
	return m, nil
}

func (m Model) handleDetailKey(key string) (tea.Model, tea.Cmd) {
	bodyHeight := max(1, m.height-3)
	switch key {
	case "q":
		return m, tea.Quit
	case "esc":
		m.view = viewProcesses
	case "up", "k":
		m.detailTop = max(0, m.detailTop-1)
	case "down", "j":
		m.detailTop = min(m.detailMaxTop(), m.detailTop+1)
	case "pgup":
		m.detailTop = max(0, m.detailTop-bodyHeight)
	case "pgdown":
		m.detailTop = min(m.detailMaxTop(), m.detailTop+bodyHeight)
	case "home", "g":
		m.detailTop = 0
	case "end", "G":
		m.detailTop = m.detailMaxTop()
	case "enter", "l":
		return m.openLogs(m.detail, model.LogStdout)
	case "a":
		return m, processActionCmd(m.ctx, m.client, "start", m.detail.Config.ID)
	case "r":
		return m, processActionCmd(m.ctx, m.client, "restart", m.detail.Config.ID)
	case "s":
		return m, processActionCmd(m.ctx, m.client, "stop", m.detail.Config.ID)
	}
	return m, nil
}

func (m Model) handleLogKey(key string) (tea.Model, tea.Cmd) {
	height := m.logBodyHeight()
	switch key {
	case "q":
		m.closeLogSubscription()
		m.view = viewProcesses
		return m, nil
	case "esc":
		if m.logs.Buffer.SelectionMode() != selectionNone {
			m.logs.Buffer.CancelSelection()
			return m, nil
		}
		m.closeLogSubscription()
		m.view = viewProcesses
		return m, nil
	case "up", "k":
		m.logs.Buffer.Follow = false
		m.logs.Buffer.MoveLines(-1, height)
		m.ensureSelectionColumnVisible()
		cmd := m.loadBeforeIfNeeded()
		return m, cmd
	case "down", "j":
		m.logs.Buffer.MoveLines(1, height)
		m.ensureSelectionColumnVisible()
		cmd := m.loadAfterIfNeeded()
		return m, cmd
	case "pgup":
		m.logs.Buffer.Follow = false
		m.logs.Buffer.MoveLines(-height, height)
		m.ensureSelectionColumnVisible()
		cmd := m.loadBeforeIfNeeded()
		return m, cmd
	case "pgdown":
		m.logs.Buffer.MoveLines(height, height)
		m.ensureSelectionColumnVisible()
		cmd := m.loadAfterIfNeeded()
		return m, cmd
	case "home":
		m.logs.Buffer.Horizontal = 0
	case "end":
		if len(m.logs.Buffer.Lines) > 0 {
			m.logs.Buffer.Horizontal = max(0, runeCount(m.logs.Buffer.Lines[m.logs.Buffer.CursorLine].Text)-max(1, m.width))
		}
	case "left", "h":
		if m.logs.Buffer.SelectionMode() == selectionCharacter {
			m.logs.Buffer.MoveColumn(-1)
			m.logs.Buffer.EnsureCursorHorizontal(max(1, m.width))
		} else {
			m.logs.Buffer.Horizontal = max(0, m.logs.Buffer.Horizontal-4)
		}
	case "right", "l":
		if m.logs.Buffer.SelectionMode() == selectionCharacter {
			m.logs.Buffer.MoveColumn(1)
			m.logs.Buffer.EnsureCursorHorizontal(max(1, m.width))
		} else {
			m.logs.Buffer.Horizontal += 4
		}
	case "g":
		if m.logs.Loading {
			return m, nil
		}
		m.logs.Buffer.Follow = false
		if m.logs.Buffer.StartOffset <= 0 {
			m.logs.Buffer.Top()
			return m, nil
		}
		m.logs.Loading = true
		return m, readLogCmd(m.ctx, m.client, m.logToken, m.logs.Process.Config.ID, m.logs.Stream, 0, MaxLogBytes, logReadTop)
	case "G":
		return m.reloadLogTail(true)
	case "f":
		if m.logs.Buffer.Follow {
			m.logs.Buffer.Follow = false
			return m, nil
		}
		return m.reloadLogTail(true)
	case "/":
		m.logs.BeginSearch()
	case "n":
		if !m.logs.Buffer.Find(m.logs.Query, 1, height) {
			m.setStatus("No matching loaded log line", true)
		}
	case "N":
		if !m.logs.Buffer.Find(m.logs.Query, -1, height) {
			m.setStatus("No matching loaded log line", true)
		}
	case "v":
		m.logs.Buffer.ToggleSelection(selectionCharacter)
	case "V":
		m.logs.Buffer.ToggleSelection(selectionLine)
	case "y":
		text := m.logs.Buffer.SelectedText()
		if text == "" {
			m.setStatus("No log text selected", true)
			return m, nil
		}
		m.lastSelection = text
		return m, copyToClipboardCmd(text)
	case "w":
		text := m.logs.Buffer.SelectedText()
		if text == "" {
			text = m.lastSelection
		}
		if text == "" {
			m.setStatus("No log text selected", true)
			return m, nil
		}
		return m, writeSelectionCmd(text)
	case "1":
		if m.logs.Stream != model.LogStdout {
			return m.switchLogStream(model.LogStdout)
		}
	case "2":
		if m.logs.Stream != model.LogStderr {
			return m.switchLogStream(model.LogStderr)
		}
	case "r":
		return m, processActionCmd(m.ctx, m.client, "restart", m.logs.Process.Config.ID)
	}
	return m, nil
}

func (m *Model) ensureSelectionColumnVisible() {
	if m.logs.Buffer.SelectionMode() == selectionCharacter {
		m.logs.Buffer.EnsureCursorHorizontal(max(1, m.width))
	}
}

func (m Model) actionForSelected(key string) (tea.Model, tea.Cmd) {
	info, ok := m.processes.Selected()
	if !ok {
		return m, nil
	}
	action := map[string]string{"a": "start", "r": "restart", "s": "stop"}[key]
	return m, processActionCmd(m.ctx, m.client, action, info.Config.ID)
}
