package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"akid/internal/model"
)

var (
	accentColor = lipgloss.Color("39")
	greenColor  = lipgloss.Color("42")
	yellowColor = lipgloss.Color("214")
	redColor    = lipgloss.Color("196")
	grayColor   = lipgloss.Color("245")

	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	subtleStyle     = lipgloss.NewStyle().Foreground(grayColor)
	errorStyle      = lipgloss.NewStyle().Foreground(redColor)
	successStyle    = lipgloss.NewStyle().Foreground(greenColor)
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("24"))
	selectionStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("220"))
	cursorLineStyle = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	matchStyle      = lipgloss.NewStyle().Foreground(yellowColor)
	statusStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("237"))
)

func renderHeader(title string, width int) string {
	left := titleStyle.Render(" akid ") + subtleStyle.Render("│ "+title)
	return fitCell(left, width)
}

func renderFooter(text string, width int) string {
	return subtleStyle.MaxWidth(width).Render(text)
}

func fitCell(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Inline(true).Width(width).MaxWidth(width).Render(value)
}

func statusText(info model.ProcessInfo) string {
	if !info.Runtime.NextRetryAt.IsZero() {
		return "backoff"
	}
	if info.Runtime.Status == "" {
		return "unknown"
	}
	return string(info.Runtime.Status)
}

func statusStyleFor(info model.ProcessInfo) lipgloss.Style {
	switch statusText(info) {
	case "running":
		return successStyle
	case "backoff", "starting", "restarting", "stopping":
		return lipgloss.NewStyle().Foreground(yellowColor)
	case "exited":
		return errorStyle
	default:
		return subtleStyle
	}
}

func formatUptime(info model.ProcessInfo, now time.Time) string {
	if info.Runtime.Status != model.StatusRunning || info.Runtime.StartedAt.IsZero() {
		return "-"
	}
	d := now.Sub(info.Runtime.StartedAt)
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d/time.Hour)%24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d/time.Minute)%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d/time.Second)%60)
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func formatBytes(size int64) string {
	if size < 0 {
		size = 0
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGT"[exp])
}

func joinCommand(info model.ProcessInfo) string {
	parts := append([]string{info.Config.Command}, info.Config.Args...)
	return strings.Join(parts, " ")
}
