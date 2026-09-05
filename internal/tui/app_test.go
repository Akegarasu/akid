package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	akidlog "akid/internal/logging"
	"akid/internal/model"
	"akid/internal/protocol"
)

type fakeClient struct {
	processes []model.ProcessInfo
	logChunk  akidlog.LogChunk
	events    chan model.Event
	logs      chan akidlog.LogEvent
}

func (f *fakeClient) Call(_ context.Context, method string, _ any, result any) error {
	switch method {
	case "process.list":
		*result.(*[]model.ProcessInfo) = append([]model.ProcessInfo(nil), f.processes...)
	case "process.metrics":
		*result.(*[]model.ProcessMetrics) = nil
	case "log.read":
		*result.(*akidlog.LogChunk) = f.logChunk
	case "process.start", "process.stop", "process.restart":
		*result.(*model.ProcessInfo) = f.processes[0]
	}
	return nil
}

func (f *fakeClient) SubscribeEvents(context.Context) (<-chan model.Event, error) {
	return f.events, nil
}

func (f *fakeClient) SubscribeLogs(context.Context, protocol.LogSubscribeParams) (<-chan akidlog.LogEvent, error) {
	return f.logs, nil
}

func TestModelLoadsAndFollowsLogs(t *testing.T) {
	info := processInfo("api", "id-api")
	client := &fakeClient{
		processes: []model.ProcessInfo{info},
		logChunk:  chunk(0, 1, "one\n"),
		events:    make(chan model.Event, 1),
		logs:      make(chan akidlog.LogEvent, 1),
	}
	m := NewModel(context.Background(), client)
	m.width, m.height = 80, 20
	updated, _ := m.Update(processEventMsg{open: true, event: model.Event{Name: "process.snapshot", Snapshot: &model.ProcessSnapshot{Processes: client.processes}}})
	m = updated.(Model)

	updated, readCmd := m.Update(keyText("enter"))
	m = updated.(Model)
	if m.view != viewLogs || readCmd == nil {
		t.Fatalf("logs were not opened: view=%d cmd=%v", m.view, readCmd)
	}
	readMsg := readCmd().(logReadMsg)
	updated, subscribeCmd := m.Update(readMsg)
	m = updated.(Model)
	if got := lineTexts(m.logs.Buffer.Lines); got != "one" {
		t.Fatalf("initial logs = %q", got)
	}

	subscribeMsg := subscribeCmd().(logSubscriptionMsg)
	updated, waitCmd := m.Update(subscribeMsg)
	m = updated.(Model)
	client.logs <- akidlog.LogEvent{Chunk: chunk(4, 1, "two\n")}
	logMsg := waitCmd().(logEventMsg)
	updated, nextWait := m.Update(logMsg)
	m = updated.(Model)
	if nextWait == nil {
		t.Fatal("log subscription did not continue")
	}
	if got := lineTexts(m.logs.Buffer.Lines); got != "one|two" {
		t.Fatalf("followed logs = %q", got)
	}
	if m.logs.Buffer.CursorLine != 1 || m.logs.Unseen != 0 {
		t.Fatalf("follow state cursor=%d unseen=%d", m.logs.Buffer.CursorLine, m.logs.Unseen)
	}
	m.closeLogSubscription()
}

func TestBrowseModePreservesWindowWhenNewLogsArrive(t *testing.T) {
	info := processInfo("api", "id-api")
	m := NewModel(context.Background(), &fakeClient{})
	m.view = viewLogs
	m.logs.Reset(info, model.LogStdout)
	m.logs.Buffer.Reset(chunk(0, 1, "one\n"), false)
	m.logToken, m.logGeneration, m.logTailOffset = 4, 1, 4
	events := make(chan akidlog.LogEvent)

	updated, next := m.Update(logEventMsg{
		token:  4,
		event:  akidlog.LogEvent{Chunk: chunk(4, 1, "two\n")},
		events: events,
		open:   true,
	})
	m = updated.(Model)
	if next == nil {
		t.Fatal("subscription did not continue")
	}
	if got := lineTexts(m.logs.Buffer.Lines); got != "one" {
		t.Fatalf("browse window changed: %q", got)
	}
	if m.logs.Unseen != 1 || m.logs.Buffer.EOF {
		t.Fatalf("new log state unseen=%d eof=%v", m.logs.Unseen, m.logs.Buffer.EOF)
	}

	updated, _ = m.Update(logReadMsg{token: 4, kind: logReadAfter, chunk: chunk(4, 1, "two\n")})
	m = updated.(Model)
	if m.logs.Unseen != 0 || lineTexts(m.logs.Buffer.Lines) != "one|two" {
		t.Fatalf("catch-up did not clear unseen count: unseen=%d lines=%q", m.logs.Unseen, lineTexts(m.logs.Buffer.Lines))
	}
}

func TestInitialLogReadFailureSchedulesReload(t *testing.T) {
	m := NewModel(context.Background(), &fakeClient{})
	m.view = viewLogs
	m.logToken = 3
	updated, cmd := m.Update(logReadMsg{token: 3, kind: logReadTail, err: errors.New("offline")})
	m = updated.(Model)
	if cmd == nil || m.status == "" {
		t.Fatalf("reload was not scheduled: cmd=%v status=%q", cmd, m.status)
	}
}

func TestModelIgnoresStaleLogMessages(t *testing.T) {
	info := processInfo("api", "id-api")
	m := NewModel(context.Background(), &fakeClient{})
	m.view = viewLogs
	m.logs.Reset(info, model.LogStdout)
	m.logToken = 9
	updated, cmd := m.Update(logReadMsg{token: 8, chunk: chunk(0, 1, "stale\n")})
	m = updated.(Model)
	if cmd != nil || len(m.logs.Buffer.Lines) != 0 {
		t.Fatal("stale log read changed the model")
	}
}

func TestProcessDetailScrollsLongEnvironment(t *testing.T) {
	info := processInfo("api", "1")
	info.Config.Env = make(map[string]string)
	for i := 0; i < 20; i++ {
		info.Config.Env[fmt.Sprintf("KEY_%02d", i)] = "value"
	}
	m := NewModel(context.Background(), &fakeClient{})
	m.width, m.height = 80, 12
	m.detail, m.view = info, viewProcessDetail
	updated, _ := m.Update(keyText("G"))
	m = updated.(Model)
	if m.detailTop == 0 || m.detailTop != m.detailMaxTop() {
		t.Fatalf("detail did not scroll to end: top=%d max=%d", m.detailTop, m.detailMaxTop())
	}
	updated, _ = m.Update(keyText("g"))
	m = updated.(Model)
	if m.detailTop != 0 {
		t.Fatalf("detail did not scroll home: %d", m.detailTop)
	}
}

func TestOptionalMouseSelectsProcessAndLogLine(t *testing.T) {
	first := processInfo("api", "1")
	second := processInfo("worker", "2")
	m := NewModel(context.Background(), &fakeClient{}, Options{Mouse: true})
	m.width, m.height = 80, 18
	m.processes.Set([]model.ProcessInfo{first, second})
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("mouse mode was not enabled")
	}

	updated, _ := m.Update(tea.MouseClickMsg(tea.Mouse{X: 2, Y: 3, Button: tea.MouseLeft}))
	m = updated.(Model)
	selected, ok := m.processes.Selected()
	if !ok || selected.Config.Name != "worker" {
		t.Fatalf("mouse selected %#v", selected)
	}

	m.view = viewLogs
	m.logs.Reset(first, model.LogStdout)
	m.logs.Buffer.Reset(chunk(0, 1, "one\ntwo\nthree\n"), true)
	updated, _ = m.Update(tea.MouseClickMsg(tea.Mouse{X: 1, Y: 2, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.logs.Buffer.CursorLine != 1 || m.logs.Buffer.CursorCol != 1 || m.logs.Buffer.Follow {
		t.Fatalf("log click cursor=%d:%d follow=%v", m.logs.Buffer.CursorLine, m.logs.Buffer.CursorCol, m.logs.Buffer.Follow)
	}
}

func TestViewsFillTerminalHeight(t *testing.T) {
	info := processInfo("api", "id-api")
	m := NewModel(context.Background(), &fakeClient{})
	m.width, m.height = 80, 18
	m.processes.Set([]model.ProcessInfo{info})
	if lines := strings.Count(m.View().Content, "\n") + 1; lines != 18 {
		t.Fatalf("process view has %d lines", lines)
	}
	m.detail, m.view = info, viewProcessDetail
	if lines := strings.Count(m.View().Content, "\n") + 1; lines != 18 {
		t.Fatalf("detail view has %d lines", lines)
	}
	m.logs.Reset(info, model.LogStdout)
	m.view = viewLogs
	if lines := strings.Count(m.View().Content, "\n") + 1; lines != 18 {
		t.Fatalf("log view has %d lines", lines)
	}
}

func keyText(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: text})
}

func processInfo(name, id string) model.ProcessInfo {
	return model.ProcessInfo{
		Config:  model.ProcessConfig{ID: id, Name: name, Command: "./server", Restart: model.RestartAlways},
		Runtime: model.RuntimeState{Status: model.StatusRunning, PID: 123},
		Desired: model.DesiredRunning,
	}
}

func TestDeletedProcessClosesLogView(t *testing.T) {
	m := NewModel(context.Background(), &fakeClient{})
	info := processInfo("api", "1")
	info.Epoch, info.Revision = "daemon", 1
	m.processes.ApplySnapshot(model.ProcessSnapshot{Epoch: "daemon", Revision: 1, Processes: []model.ProcessInfo{info}})
	m.view = viewLogs
	m.logs.Reset(info, model.LogStdout)
	m.logCtx, m.logCancel = context.WithCancel(m.ctx)
	logCtx := m.logCtx
	info.Revision = 2
	updated, _ := m.Update(processEventMsg{open: true, event: model.Event{Name: "process.deleted", Data: info}})
	m = updated.(Model)
	if m.view != viewProcesses || len(m.processes.all) != 0 || logCtx.Err() == nil {
		t.Fatal("deleted process view remained open")
	}
}

func TestMouseSelectsWideCharacterByDisplayCell(t *testing.T) {
	m := NewModel(context.Background(), &fakeClient{}, Options{Mouse: true})
	m.width, m.height = 20, 10
	m.view = viewLogs
	m.logs.Buffer.Reset(chunk(0, 1, "a\u4e2db\n"), false)
	updated, _ := m.Update(tea.MouseClickMsg(tea.Mouse{X: 2, Y: 1, Button: tea.MouseLeft}))
	m = updated.(Model)
	if m.logs.Buffer.CursorCol != 1 {
		t.Fatalf("wide character click landed at rune %d", m.logs.Buffer.CursorCol)
	}
}
