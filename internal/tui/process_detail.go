package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"akid/internal/model"
)

func renderProcessDetail(info model.ProcessInfo, metric model.ProcessMetrics, top, width, height int, now time.Time, status string, statusError bool) string {
	if width < 1 {
		width = 1
	}
	if height < 3 {
		height = 3
	}
	rows := processDetailRows(info, metric, width, now)
	bodyHeight := max(1, height-3)
	top = clamp(top, 0, max(0, len(rows)-bodyHeight))
	end := min(len(rows), top+bodyHeight)
	visible := append([]string(nil), rows[top:end]...)
	for len(visible) < bodyHeight {
		visible = append(visible, "")
	}

	title := "Process: " + info.Config.Name
	if len(rows) > bodyHeight {
		title += fmt.Sprintf("  [%d-%d/%d]", top+1, end, len(rows))
	}
	message := ""
	if status != "" {
		style := successStyle
		if statusError {
			style = errorStyle
		}
		message = style.Render(status)
	}
	return strings.Join([]string{
		renderHeader(title, width),
		strings.Join(visible, "\n"),
		statusStyle.Width(width).MaxWidth(width).Render(message),
		renderFooter("↑↓/jk scroll  l/Enter logs  a start  r restart  s stop  Esc back", width),
	}, "\n")
}

func processDetailRows(info model.ProcessInfo, metric model.ProcessMetrics, width int, now time.Time) []string {
	pid := "-"
	if info.Runtime.PID > 0 {
		pid = strconv.Itoa(info.Runtime.PID)
	}
	exitCode := "-"
	if info.Runtime.ExitCode != nil {
		exitCode = strconv.Itoa(*info.Runtime.ExitCode)
	}
	cpu, memory := "-", "-"
	if metric.Available && metric.PID == info.Runtime.PID {
		memory = formatBytes(int64(metric.MemoryBytes))
		if metric.CPUAvailable {
			cpu = fmt.Sprintf("%.1f%%", metric.CPUPercent)
		}
	}
	rows := []string{
		detailRow("ID", info.Config.ID, width),
		detailRow("Status", statusStyleFor(info).Render(statusText(info)), width),
		detailRow("Desired", string(info.Desired), width),
		detailRow("PID", pid, width),
		detailRow("Uptime", formatUptime(info, now), width),
		detailRow("CPU", cpu, width),
		detailRow("Memory", memory, width),
		detailRow("Restarts", strconv.FormatUint(info.Runtime.RestartCount, 10), width),
		detailRow("Exit code", exitCode, width),
		detailRow("Command", info.Config.Command, width),
		detailRow("Args", strings.Join(info.Config.Args, " "), width),
		detailRow("CWD", info.Config.Cwd, width),
		detailRow("Restart", string(info.Config.Restart), width),
		detailRow("Stop timeout", info.Config.StopTimeout().String(), width),
		detailRow("Started", formatTime(info.Runtime.StartedAt), width),
		detailRow("Stopped", formatTime(info.Runtime.StoppedAt), width),
		detailRow("Log stdout", strconv.FormatUint(info.OutGeneration, 10), width),
		detailRow("Log stderr", strconv.FormatUint(info.ErrGeneration, 10), width),
		"",
		titleStyle.Render("Environment"),
	}
	keys := make([]string, 0, len(info.Config.Env))
	for key := range info.Config.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rows = append(rows, fitCell(fmt.Sprintf("%s=%s", key, info.Config.Env[key]), width))
	}
	if len(keys) == 0 {
		rows = append(rows, subtleStyle.Render("(inherited environment only)"))
	}
	return rows
}

func (m Model) detailMaxTop() int {
	bodyHeight := max(1, m.height-3)
	metric := m.processes.metrics[m.detail.Config.ID]
	return max(0, len(processDetailRows(m.detail, metric, max(1, m.width), m.now))-bodyHeight)
}

func detailRow(label, value string, width int) string {
	if value == "" {
		value = "-"
	}
	return fitCell(fmt.Sprintf("%-14s %s", label, value), width)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}
