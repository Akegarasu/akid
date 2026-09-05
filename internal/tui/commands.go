package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	akidlog "akid/internal/logging"
	"akid/internal/model"
	"akid/internal/protocol"
)

const callTimeout = 30 * time.Second

// Client is the protocol surface used by the TUI. Keeping this interface here
// makes the UI testable and prevents it from reaching into daemon internals.
type Client interface {
	Call(ctx context.Context, method string, params any, result any) error
	SubscribeEvents(ctx context.Context) (<-chan model.Event, error)
	SubscribeLogs(ctx context.Context, params protocol.LogSubscribeParams) (<-chan akidlog.LogEvent, error)
}

type metricsLoadedMsg struct {
	metrics []model.ProcessMetrics
	err     error
}

type eventSubscriptionMsg struct {
	events <-chan model.Event
	err    error
}

type processEventMsg struct {
	event  model.Event
	events <-chan model.Event
	open   bool
}

type reconnectEventsMsg struct{}

type processActionMsg struct {
	action string
	info   model.ProcessInfo
	err    error
}

type logReadKind uint8

const (
	logReadTail logReadKind = iota
	logReadTop
	logReadBefore
	logReadAfter
)

type logReadMsg struct {
	token uint64
	kind  logReadKind
	chunk akidlog.LogChunk
	err   error
}

type logSubscriptionMsg struct {
	token  uint64
	events <-chan akidlog.LogEvent
	err    error
}

type logEventMsg struct {
	token  uint64
	event  akidlog.LogEvent
	events <-chan akidlog.LogEvent
	open   bool
}

type reconnectLogMsg struct{ token uint64 }
type reloadLogMsg struct{ token uint64 }
type tickMsg time.Time

type clipboardResultMsg struct {
	method string
	err    error
	text   string
}

type selectionWrittenMsg struct {
	path string
	err  error
}

func loadMetricsCmd(ctx context.Context, client Client) tea.Cmd {
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		var metrics []model.ProcessMetrics
		err := client.Call(callCtx, "process.metrics", nil, &metrics)
		return metricsLoadedMsg{metrics: metrics, err: err}
	}
}

func subscribeEventsCmd(ctx context.Context, client Client) tea.Cmd {
	return func() tea.Msg {
		events, err := client.SubscribeEvents(ctx)
		return eventSubscriptionMsg{events: events, err: err}
	}
}

func waitProcessEventCmd(events <-chan model.Event) tea.Cmd {
	return func() tea.Msg {
		event, open := <-events
		return processEventMsg{event: event, events: events, open: open}
	}
}

func processActionCmd(ctx context.Context, client Client, action, id string) tea.Cmd {
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		var info model.ProcessInfo
		err := client.Call(callCtx, "process."+action, map[string]string{"id": id}, &info)
		return processActionMsg{action: action, info: info, err: err}
	}
}

func readLogCmd(ctx context.Context, client Client, token uint64, id string, stream model.LogStream, offset int64, limit int, kind logReadKind) tea.Cmd {
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		var chunk akidlog.LogChunk
		err := client.Call(callCtx, "log.read", map[string]any{
			"id": id, "stream": stream, "offset": offset, "limit": limit,
			"align": kind == logReadTail,
		}, &chunk)
		return logReadMsg{token: token, kind: kind, chunk: chunk, err: err}
	}
}

func subscribeLogsCmd(ctx context.Context, client Client, token uint64, params protocol.LogSubscribeParams) tea.Cmd {
	return func() tea.Msg {
		events, err := client.SubscribeLogs(ctx, params)
		return logSubscriptionMsg{token: token, events: events, err: err}
	}
}

func waitLogEventCmd(token uint64, events <-chan akidlog.LogEvent) tea.Cmd {
	return func() tea.Msg {
		event, open := <-events
		return logEventMsg{token: token, event: event, events: events, open: open}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}
