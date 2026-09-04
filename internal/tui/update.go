package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"akid/internal/model"
)

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case processesLoadedMsg:
		if msg.err != nil {
			m.setStatus("Cannot load processes: "+msg.err.Error(), true)
			return m, nil
		}
		m.processes.Set(msg.processes)
		m.syncCurrentProcess()
		return m, nil
	case metricsLoadedMsg:
		m.metricsPending = false
		if msg.err != nil {
			if m.status == "" {
				m.setStatus("Cannot load process metrics: "+msg.err.Error(), true)
			}
			return m, nil
		}
		m.processes.SetMetrics(msg.metrics)
		return m, nil
	case eventSubscriptionMsg:
		if msg.err != nil {
			m.setStatus("Status events disconnected: "+msg.err.Error(), true)
			return m, reconnectEventsAfter()
		}
		// Reconcile after every (re)connect because events may have been
		// missed while the subscription was down.
		return m, tea.Batch(waitProcessEventCmd(msg.events), loadProcessesCmd(m.ctx, m.client))
	case processEventMsg:
		if !msg.open {
			return m, reconnectEventsAfter()
		}
		cmds := []tea.Cmd{waitProcessEventCmd(msg.events)}
		if msg.event.Name == "event.lagged" {
			m.setStatus("Status events lagged; refreshing", true)
			cmds = append(cmds, loadProcessesCmd(m.ctx, m.client))
			return m, tea.Batch(cmds...)
		}
		m.processes.Upsert(msg.event.Data)
		m.updateCurrentProcess(msg.event.Data)
		return m, tea.Batch(cmds...)
	case reconnectEventsMsg:
		return m, subscribeEventsCmd(m.ctx, m.client)
	case processActionMsg:
		if msg.err != nil {
			m.setStatus(msg.action+" failed: "+msg.err.Error(), true)
			return m, nil
		}
		m.processes.Upsert(msg.info)
		m.updateCurrentProcess(msg.info)
		m.setStatus(msg.action+" requested for "+msg.info.Config.Name, false)
		return m, nil
	case logReadMsg:
		return m.handleLogRead(msg)
	case logSubscriptionMsg:
		if msg.token != m.logToken || m.view != viewLogs {
			return m, nil
		}
		if msg.err != nil {
			m.setStatus("Log stream disconnected: "+msg.err.Error(), true)
			return m, reconnectLogAfter(msg.token)
		}
		return m, waitLogEventCmd(msg.token, msg.events)
	case logEventMsg:
		return m.handleLogEvent(msg)
	case reconnectLogMsg:
		if msg.token != m.logToken || m.view != viewLogs {
			return m, nil
		}
		return m, m.subscribeCurrentLog()
	case reloadLogMsg:
		if msg.token != m.logToken || m.view != viewLogs {
			return m, nil
		}
		return m.reloadLogTail(m.logs.Buffer.Follow)
	case clipboardResultMsg:
		if msg.err == nil {
			m.logs.Buffer.CancelSelection()
			m.setStatus("Copied selection via "+msg.method, false)
			return m, nil
		}
		// OSC 52 has no acknowledgement, but sending the escape sequence is a
		// useful fallback for SSH and modern terminal emulators.
		m.logs.Buffer.CancelSelection()
		m.setStatus("Sent selection via OSC 52; press w to write a file", false)
		return m, tea.SetClipboard(msg.text)
	case selectionWrittenMsg:
		if msg.err != nil {
			m.setStatus("Cannot write selection: "+msg.err.Error(), true)
			return m, nil
		}
		m.logs.Buffer.CancelSelection()
		m.setStatus("Selection written to "+msg.path, false)
		return m, nil
	case tickMsg:
		m.now = time.Time(msg)
		if !m.statusUntil.IsZero() && !m.now.Before(m.statusUntil) {
			m.status, m.statusError, m.statusUntil = "", false, time.Time{}
		}
		cmds := []tea.Cmd{tickCmd()}
		if !m.metricsPending {
			m.metricsPending = true
			cmds = append(cmds, loadMetricsCmd(m.ctx, m.client))
		}
		return m, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) syncCurrentProcess() {
	if m.view == viewProcessDetail {
		for _, info := range m.processes.all {
			if info.Config.ID == m.detail.Config.ID {
				m.detail = info
				return
			}
		}
	}
	if m.view == viewLogs {
		for _, info := range m.processes.all {
			if info.Config.ID == m.logs.Process.Config.ID {
				m.logs.Process = info
				return
			}
		}
	}
}

func (m *Model) updateCurrentProcess(info model.ProcessInfo) {
	if m.view == viewProcessDetail && m.detail.Config.ID == info.Config.ID {
		m.detail = info
	}
	if m.view == viewLogs && m.logs.Process.Config.ID == info.Config.ID {
		m.logs.Process = info
	}
}

func reconnectEventsAfter() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return reconnectEventsMsg{} })
}

func reconnectLogAfter(token uint64) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return reconnectLogMsg{token: token} })
}

func reloadLogAfter(token uint64) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return reloadLogMsg{token: token} })
}
