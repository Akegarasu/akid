package tui

import (
	"bytes"
	"context"

	tea "charm.land/bubbletea/v2"

	"akid/internal/model"
	"akid/internal/protocol"
)

func (m Model) openLogs(info model.ProcessInfo, stream model.LogStream) (tea.Model, tea.Cmd) {
	m.view = viewLogs
	m.logs.Reset(info, stream)
	return m.reloadLogTail(true)
}

func (m Model) switchLogStream(stream model.LogStream) (tea.Model, tea.Cmd) {
	m.logs.Reset(m.logs.Process, stream)
	return m.reloadLogTail(true)
}

func (m Model) reloadLogTail(follow bool) (tea.Model, tea.Cmd) {
	m.closeLogSubscription()
	m.logCtx, m.logCancel = context.WithCancel(m.ctx)
	m.logs.Buffer = NewLogBuffer()
	m.logs.Buffer.Follow = follow
	m.logs.Loading = true
	m.logs.Unseen = 0
	return m, readLogCmd(m.ctx, m.client, m.logToken, m.logs.Process.Config.ID, m.logs.Stream, -MaxLogBytes, MaxLogBytes, logReadTail)
}

func (m *Model) loadBeforeIfNeeded() tea.Cmd {
	if m.logs.Loading || m.logs.Buffer.CursorLine > 1 || m.logs.Buffer.StartOffset <= 0 {
		return nil
	}
	offset := max(int64(0), m.logs.Buffer.StartOffset-LogPageSize)
	limit := int(m.logs.Buffer.StartOffset - offset)
	if limit <= 0 {
		return nil
	}
	m.logs.Loading = true
	return readLogCmd(m.ctx, m.client, m.logToken, m.logs.Process.Config.ID, m.logs.Stream, offset, limit, logReadBefore)
}

func (m *Model) loadAfterIfNeeded() tea.Cmd {
	if m.logs.Loading || m.logs.Buffer.EOF || len(m.logs.Buffer.Lines) == 0 || m.logs.Buffer.CursorLine < len(m.logs.Buffer.Lines)-2 {
		return nil
	}
	m.logs.Loading = true
	return readLogCmd(m.ctx, m.client, m.logToken, m.logs.Process.Config.ID, m.logs.Stream, m.logs.Buffer.EndOffset, LogPageSize, logReadAfter)
}

func (m Model) handleLogRead(msg logReadMsg) (tea.Model, tea.Cmd) {
	if msg.token != m.logToken || m.view != viewLogs {
		return m, nil
	}
	m.logs.Loading = false
	if msg.err != nil {
		m.setStatus("Cannot read logs: "+msg.err.Error(), true)
		if msg.kind == logReadTail {
			return m, reloadLogAfter(msg.token)
		}
		return m, nil
	}
	switch msg.kind {
	case logReadTail:
		m.logs.Buffer.reset(msg.chunk, m.logs.Buffer.Follow, false)
		m.logs.Buffer.ensureVisible(m.logBodyHeight())
		m.logTailOffset = msg.chunk.EndOffset
		m.logGeneration = msg.chunk.Generation
		return m, m.subscribeCurrentLog()
	case logReadTop:
		if msg.chunk.Generation != m.logGeneration {
			return m.reloadLogTail(m.logs.Buffer.Follow)
		}
		m.logs.Buffer.Reset(msg.chunk, false)
		m.logs.Buffer.Top()
	case logReadBefore:
		if !m.logs.Buffer.Prepend(msg.chunk) {
			return m.reloadLogTail(m.logs.Buffer.Follow)
		}
	case logReadAfter:
		if !m.logs.Buffer.Append(msg.chunk) {
			return m.reloadLogTail(m.logs.Buffer.Follow)
		}
		if m.logs.Buffer.EndOffset >= m.logTailOffset {
			m.logs.Unseen = 0
		}
	}
	return m, nil
}

func (m Model) handleLogEvent(msg logEventMsg) (tea.Model, tea.Cmd) {
	if msg.token != m.logToken || m.view != viewLogs {
		return m, nil
	}
	if !msg.open {
		return m, reconnectLogAfter(msg.token)
	}
	if msg.event.Lagged {
		m.setStatus("Log stream lagged; resynchronizing", true)
		return m.reloadLogTail(m.logs.Buffer.Follow)
	}
	if msg.event.Gap {
		m.setStatus("Log continuity lost; reloading active file", true)
		return m.reloadLogTail(m.logs.Buffer.Follow)
	}
	chunk := msg.event.Chunk
	if chunk.Generation != m.logGeneration {
		m.setStatus("Log rotated; reloading", false)
		return m.reloadLogTail(m.logs.Buffer.Follow)
	}
	m.logTailOffset = max(m.logTailOffset, chunk.EndOffset)
	lineCount := bytes.Count(chunk.Data, []byte{'\n'})
	if lineCount == 0 && len(chunk.Data) > 0 {
		lineCount = 1
	}
	if !m.logs.Buffer.Follow {
		// Preserve the historical viewport. PageDown can fetch the gap, while
		// G/f intentionally rebuilds the window at the current tail.
		m.logs.Unseen += lineCount
		m.logs.Buffer.EOF = false
	} else if chunk.StartOffset == m.logs.Buffer.EndOffset {
		if !m.logs.Buffer.Append(chunk) {
			return m.reloadLogTail(true)
		}
		m.logs.Buffer.ensureVisible(m.logBodyHeight())
		m.logs.Unseen = 0
	} else {
		return m.reloadLogTail(true)
	}
	return m, waitLogEventCmd(msg.token, msg.events)
}

func (m Model) subscribeCurrentLog() tea.Cmd {
	return subscribeLogsCmd(m.logCtx, m.client, m.logToken, protocol.LogSubscribeParams{
		ID: m.logs.Process.Config.ID, Stream: m.logs.Stream,
		Offset: m.logTailOffset, Generation: m.logGeneration,
	})
}

func (m *Model) closeLogSubscription() {
	if m.logCancel != nil {
		m.logCancel()
		m.logCancel = nil
	}
	m.logToken++
}
