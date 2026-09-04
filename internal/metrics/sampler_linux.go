//go:build linux

package metrics

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"

	"akid/internal/model"
)

type processSample struct {
	startTime uint64
	cpuTicks  uint64
	rssPages  int64
}

func (s *Sampler) Sample(processes []model.ProcessInfo) []model.ProcessMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()

	total, totalOK := readTotalCPUTicks()
	totalDelta := uint64(0)
	if totalOK && s.lastTotal > 0 && total >= s.lastTotal {
		totalDelta = total - s.lastTotal
	}
	if totalOK {
		s.lastTotal = total
	}

	result := make([]model.ProcessMetrics, 0, len(processes))
	next := make(map[string]previousProcess, len(processes))
	for _, info := range processes {
		metric := model.ProcessMetrics{ID: info.Config.ID, PID: info.Runtime.PID}
		if info.Runtime.PID <= 0 || info.Runtime.StartTime == 0 {
			result = append(result, metric)
			continue
		}
		sample, err := readProcessSample(info.Runtime.PID)
		if err != nil || sample.startTime != info.Runtime.StartTime {
			result = append(result, metric)
			continue
		}
		metric.Available = true
		if sample.rssPages > 0 {
			metric.MemoryBytes = uint64(sample.rssPages) * uint64(os.Getpagesize())
		}
		previous, found := s.previous[info.Config.ID]
		if found && previous.pid == info.Runtime.PID && previous.startTime == sample.startTime &&
			totalDelta > 0 && sample.cpuTicks >= previous.cpuTicks {
			processDelta := sample.cpuTicks - previous.cpuTicks
			metric.CPUPercent = float64(processDelta) / float64(totalDelta) * float64(runtime.NumCPU()) * 100
			maximum := float64(runtime.NumCPU() * 100)
			if metric.CPUPercent > maximum {
				metric.CPUPercent = maximum
			}
			metric.CPUAvailable = true
		}
		next[info.Config.ID] = previousProcess{pid: info.Runtime.PID, startTime: sample.startTime, cpuTicks: sample.cpuTicks}
		result = append(result, metric)
	}
	s.previous = next
	return result
}

func readTotalCPUTicks() (uint64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	return parseTotalCPUTicks(string(data))
}

func parseTotalCPUTicks(data string) (uint64, bool) {
	line, _, _ := strings.Cut(data, "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "cpu" {
		return 0, false
	}
	var total uint64
	// guest and guest_nice are already included in user and nice. Sum through
	// steal (the first eight numeric fields) to avoid counting them twice.
	for _, field := range fields[1:min(len(fields), 9)] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, false
		}
		total += value
	}
	return total, true
}

func readProcessSample(pid int) (processSample, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return processSample{}, err
	}
	return parseProcessStat(string(data))
}

func parseProcessStat(line string) (processSample, error) {
	endComm := strings.LastIndexByte(line, ')')
	if endComm < 0 || endComm+2 >= len(line) {
		return processSample{}, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(line[endComm+2:])
	// fields[0] is field 3. utime, stime, starttime and rss are fields
	// 14, 15, 22 and 24 respectively.
	if len(fields) <= 21 {
		return processSample{}, errors.New("short /proc stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return processSample{}, os.ErrNotExist
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return processSample{}, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return processSample{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processSample{}, err
	}
	rss, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return processSample{}, err
	}
	return processSample{startTime: startTime, cpuTicks: utime + stime, rssPages: rss}, nil
}
