package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"akid/internal/model"
)

type viewKind uint8

const (
	viewProcesses viewKind = iota
	viewProcessDetail
	viewLogs
)

type Options struct {
	Mouse bool
}

type Model struct {
	ctx    context.Context
	client Client

	view      viewKind
	width     int
	height    int
	now       time.Time
	processes processList
	detail    model.ProcessInfo
	detailTop int
	logs      LogViewer

	status      string
	statusError bool
	statusUntil time.Time

	logToken       uint64
	logCtx         context.Context
	logCancel      context.CancelFunc
	logTailOffset  int64
	logGeneration  uint64
	metricsPending bool
	lastSelection  string
	mouseEnabled   bool
}

func NewModel(ctx context.Context, client Client, options ...Options) Model {
	if ctx == nil {
		ctx = context.Background()
	}
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	return Model{
		ctx:            ctx,
		client:         client,
		now:            time.Now(),
		processes:      newProcessList(),
		logs:           NewLogViewer(),
		metricsPending: true,
		mouseEnabled:   opts.Mouse,
	}
}

func Run(ctx context.Context, client Client, output io.Writer, options Options) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	program := tea.NewProgram(NewModel(runCtx, client, options), tea.WithContext(runCtx), tea.WithOutput(output))
	_, err := program.Run()
	if errors.Is(err, tea.ErrInterrupted) || (errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil) {
		return nil
	}
	return err
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(loadMetricsCmd(m.ctx, m.client), subscribeEventsCmd(m.ctx, m.client), tickCmd())
}

func (m Model) View() tea.View {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	var content string
	switch m.view {
	case viewProcessDetail:
		content = renderProcessDetail(m.detail, m.processes.metrics[m.detail.Config.ID], m.detailTop, width, height, m.now, m.status, m.statusError)
	case viewLogs:
		content = m.logs.Render(width, height, m.status, m.statusError)
	default:
		content = m.processes.Render(width, height, m.now, m.status, m.statusError)
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "akid"
	if m.mouseEnabled {
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
}

func (m *Model) setStatus(text string, isError bool) {
	m.status = text
	m.statusError = isError
	m.statusUntil = time.Now().Add(4 * time.Second)
}

func (m Model) logBodyHeight() int { return max(1, m.height-3) }

func (m Model) String() string {
	return fmt.Sprintf("view=%d processes=%d status=%s", m.view, len(m.processes.all), strings.TrimSpace(m.status))
}
