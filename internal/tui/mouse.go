package tui

import tea "charm.land/bubbletea/v2"

func (m Model) handleMouse(message tea.MouseMsg) (tea.Model, tea.Cmd) {
	mouse := message.Mouse()
	if _, clicked := message.(tea.MouseClickMsg); clicked {
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		switch m.view {
		case viewProcesses:
			// Header is row 0 and the table heading is row 1. The final two
			// rows are status and help.
			if mouse.Y >= 2 && mouse.Y < m.height-2 {
				index := m.processes.viewTop + mouse.Y - 2
				if index >= 0 && index < len(m.processes.visible) {
					m.processes.selected = index
				}
			}
		case viewLogs:
			// Log body starts below the single-line header.
			if mouse.Y >= 1 && mouse.Y < m.height-2 {
				index := m.logs.Buffer.ViewTop + mouse.Y - 1
				if index >= 0 && index < len(m.logs.Buffer.Lines) {
					m.logs.Buffer.CursorLine = index
					m.logs.Buffer.CursorCol = textRuneAtCell(m.logs.Buffer.Lines[index].Text, m.logs.Buffer.Horizontal+mouse.X)
					m.logs.Buffer.Follow = false
				}
			}
		}
		return m, nil
	}

	if _, wheel := message.(tea.MouseWheelMsg); !wheel {
		return m, nil
	}
	switch m.view {
	case viewProcessDetail:
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.detailTop = max(0, m.detailTop-3)
		case tea.MouseWheelDown:
			m.detailTop = min(m.detailMaxTop(), m.detailTop+3)
		}
	case viewProcesses:
		height := max(1, m.height-4)
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.processes.Move(-3, height)
		case tea.MouseWheelDown:
			m.processes.Move(3, height)
		}
	case viewLogs:
		height := m.logBodyHeight()
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.logs.Buffer.Follow = false
			m.logs.Buffer.MoveLines(-3, height)
			m.ensureSelectionColumnVisible()
			cmd := m.loadBeforeIfNeeded()
			return m, cmd
		case tea.MouseWheelDown:
			m.logs.Buffer.MoveLines(3, height)
			m.ensureSelectionColumnVisible()
			cmd := m.loadAfterIfNeeded()
			return m, cmd
		case tea.MouseWheelLeft:
			m.logs.Buffer.Horizontal = max(0, m.logs.Buffer.Horizontal-4)
		case tea.MouseWheelRight:
			m.logs.Buffer.Horizontal += 4
		}
	}
	return m, nil
}
