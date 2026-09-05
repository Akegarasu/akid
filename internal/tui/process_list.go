package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	"akid/internal/model"
)

type processList struct {
	epoch            string
	snapshotRevision uint64
	versions         map[string]uint64
	all              []model.ProcessInfo
	visible          []model.ProcessInfo
	selected         int
	viewTop          int
	query            string
	searching        bool
	searchInput      textinput.Model
	searchOriginal   string
	metrics          map[string]model.ProcessMetrics
}

func newProcessList() processList {
	input := textinput.New()
	input.Prompt = "/ "
	input.CharLimit = 256
	input.SetWidth(40)
	return processList{searchInput: input, metrics: make(map[string]model.ProcessMetrics), versions: make(map[string]uint64)}
}

func (l *processList) ApplySnapshot(snapshot model.ProcessSnapshot) {
	items := append([]model.ProcessInfo(nil), snapshot.Processes...)
	if snapshot.Epoch == l.epoch {
		if snapshot.Revision < l.snapshotRevision {
			return
		}
		kept := items[:0]
		for _, info := range items {
			if l.versions[info.Config.ID] <= snapshot.Revision {
				kept = append(kept, info)
			}
		}
		items = kept
		for _, info := range l.all {
			if info.Revision > snapshot.Revision {
				items = append(items, info)
			}
		}
	} else {
		clear(l.versions)
	}
	l.epoch, l.snapshotRevision = snapshot.Epoch, snapshot.Revision
	for id, revision := range l.versions {
		if revision <= snapshot.Revision {
			delete(l.versions, id)
		}
	}
	l.Set(items)
}

func (l *processList) ApplyChange(info model.ProcessInfo, deleted bool) bool {
	if info.Epoch != l.epoch || info.Revision <= max(l.snapshotRevision, l.versions[info.Config.ID]) {
		return false
	}
	l.versions[info.Config.ID] = info.Revision
	if !deleted {
		l.Upsert(info)
		return true
	}
	items := make([]model.ProcessInfo, 0, len(l.all))
	for _, old := range l.all {
		if old.Config.ID != info.Config.ID {
			items = append(items, old)
		}
	}
	l.Set(items)
	delete(l.metrics, info.Config.ID)
	return true
}

func (l *processList) Set(processes []model.ProcessInfo) {
	selectedID := l.SelectedID()
	l.all = append(l.all[:0], processes...)
	sort.Slice(l.all, func(i, j int) bool {
		return strings.ToLower(l.all[i].Config.Name) < strings.ToLower(l.all[j].Config.Name)
	})
	l.applyFilter(selectedID)
}

func (l *processList) Upsert(info model.ProcessInfo) {
	found := false
	for i := range l.all {
		if l.all[i].Config.ID == info.Config.ID {
			l.all[i] = info
			found = true
			break
		}
	}
	if !found {
		l.all = append(l.all, info)
	}
	l.Set(l.all)
}

func (l *processList) SetMetrics(values []model.ProcessMetrics) {
	clear(l.metrics)
	for _, value := range values {
		l.metrics[value.ID] = value
	}
}

func (l *processList) BeginSearch() {
	l.searching = true
	l.searchOriginal = l.query
	l.searchInput.SetValue(l.query)
	l.searchInput.SetCursor(len([]rune(l.query)))
	l.searchInput.Focus()
}

func (l *processList) UpdateSearch(value string) {
	selectedID := l.SelectedID()
	l.query = strings.TrimSpace(value)
	l.applyFilter(selectedID)
}

func (l *processList) EndSearch(commit bool) {
	if !commit {
		l.query = l.searchOriginal
	}
	l.searching = false
	l.searchInput.Blur()
	l.applyFilter("")
}

func (l *processList) Move(delta, height int) {
	if len(l.visible) == 0 {
		return
	}
	l.selected = clamp(l.selected+delta, 0, len(l.visible)-1)
	l.ensureVisible(height)
}

func (l *processList) Home(height int) {
	l.selected = 0
	l.ensureVisible(height)
}

func (l *processList) End(height int) {
	if len(l.visible) > 0 {
		l.selected = len(l.visible) - 1
	}
	l.ensureVisible(height)
}

func (l *processList) Selected() (model.ProcessInfo, bool) {
	if l.selected < 0 || l.selected >= len(l.visible) {
		return model.ProcessInfo{}, false
	}
	return l.visible[l.selected], true
}

func (l *processList) SelectedID() string {
	info, ok := l.Selected()
	if !ok {
		return ""
	}
	return info.Config.ID
}

func (l *processList) applyFilter(selectedID string) {
	needle := strings.ToLower(l.query)
	l.visible = l.visible[:0]
	for _, info := range l.all {
		if needle == "" || strings.Contains(strings.ToLower(info.Config.Name), needle) ||
			strings.Contains(strings.ToLower(info.Config.Command), needle) {
			l.visible = append(l.visible, info)
		}
	}
	l.selected = clamp(l.selected, 0, max(0, len(l.visible)-1))
	if selectedID != "" {
		for i := range l.visible {
			if l.visible[i].Config.ID == selectedID {
				l.selected = i
				break
			}
		}
	}
	if len(l.visible) == 0 {
		l.selected, l.viewTop = 0, 0
	}
}

func (l *processList) ensureVisible(height int) {
	if height < 1 {
		height = 1
	}
	if l.selected < l.viewTop {
		l.viewTop = l.selected
	}
	if l.selected >= l.viewTop+height {
		l.viewTop = l.selected - height + 1
	}
	l.viewTop = clamp(l.viewTop, 0, max(0, len(l.visible)-height))
}

func (l *processList) Render(width, height int, now time.Time, status string, statusError bool) string {
	if width < 1 {
		width = 1
	}
	if height < 4 {
		height = 4
	}
	bodyHeight := max(1, height-3)
	l.ensureVisible(max(1, bodyHeight-1))
	running := 0
	for _, info := range l.all {
		if info.Runtime.Status == model.StatusRunning {
			running++
		}
	}
	title := fmt.Sprintf("Processes                                      %d processes │ %d running", len(l.all), running)
	header := renderHeader(title, width)
	columns := processColumns(width)
	tableHeader := renderProcessRow(columns, []string{"", "NAME", "STATUS", "PID", "UPTIME", "CPU", "MEM", "RESTARTS"}, nil)

	rows := make([]string, 0, bodyHeight)
	rows = append(rows, subtleStyle.Render(tableHeader))
	visibleHeight := max(0, bodyHeight-1)
	end := min(len(l.visible), l.viewTop+visibleHeight)
	for i := l.viewTop; i < end; i++ {
		info := l.visible[i]
		pid := "-"
		if info.Runtime.PID > 0 {
			pid = strconv.Itoa(info.Runtime.PID)
		}
		marker := " "
		if i == l.selected {
			marker = ">"
		}
		cpu, memory := "-", "-"
		if metric, ok := l.metrics[info.Config.ID]; ok && metric.PID == info.Runtime.PID && metric.Available {
			memory = formatBytes(int64(metric.MemoryBytes))
			if metric.CPUAvailable {
				cpu = fmt.Sprintf("%.1f%%", metric.CPUPercent)
			}
		}
		values := []string{marker, info.Config.Name, statusText(info), pid, formatUptime(info, now), cpu, memory, strconv.FormatUint(info.Runtime.RestartCount, 10)}
		row := renderProcessRow(columns, values, &info)
		if i == l.selected {
			row = selectedStyle.Width(width).MaxWidth(width).Render(row)
		}
		rows = append(rows, row)
	}
	if len(l.visible) == 0 {
		message := "No managed processes"
		if l.query != "" {
			message = "No processes match /" + l.query
		}
		rows = append(rows, subtleStyle.Render(message))
	}
	for len(rows) < bodyHeight {
		rows = append(rows, "")
	}

	var statusLine string
	if l.searching {
		l.searchInput.SetWidth(max(1, width-2))
		statusLine = l.searchInput.View()
	} else if status != "" {
		style := successStyle
		if statusError {
			style = errorStyle
		}
		statusLine = style.Render(status)
	} else if l.query != "" {
		statusLine = subtleStyle.Render("filter: /" + l.query)
	}
	statusLine = statusStyle.Width(width).MaxWidth(width).Render(statusLine)
	help := "↑↓/jk select  Enter logs  i detail  a start  r restart  s stop  / filter  q quit"
	if l.searching {
		help = "Enter apply filter  Esc cancel"
	}
	return strings.Join([]string{header, strings.Join(rows, "\n"), statusLine, renderFooter(help, width)}, "\n")
}

func processColumns(width int) []int {
	// marker, name, status, pid, uptime, cpu, mem, restarts
	if width < 40 {
		return []int{2, max(1, width-14), 12}
	}
	if width < 72 {
		return []int{2, max(8, width-32), 11, 8, 0, 0, 0, 8}
	}
	return []int{2, max(12, width-59), 11, 8, 10, 7, 9, 8}
}

func renderProcessRow(widths []int, values []string, info *model.ProcessInfo) string {
	cells := make([]string, 0, len(widths))
	for i, width := range widths {
		if width <= 0 {
			continue
		}
		value := values[i]
		if i == 2 && info != nil {
			value = statusStyleFor(*info).Render(value)
		}
		cells = append(cells, lipgloss.NewStyle().Inline(true).Width(width).MaxWidth(width).Render(value))
	}
	return strings.Join(cells, "")
}
