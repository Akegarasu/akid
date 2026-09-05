//go:build linux

package executor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"akid/internal/model"
	"golang.org/x/sys/unix"
)

type LinuxExecutor struct {
	mu       sync.Mutex
	watchers map[int]*groupWatch
	sigchld  chan os.Signal
	closed   chan struct{}
	finished chan struct{}
	once     sync.Once
}

type groupWatch struct {
	startTime   uint64
	done        chan ExitResult
	adopted     bool
	members     map[int]uint64
	stopTimeout time.Duration
	cleanupAt   time.Time
	emptySeen   bool
}

type processStat struct {
	pid, parent, group int
	startTime          uint64
	state              string
}

func (s processStat) live() bool { return s.state != "Z" && s.state != "X" }

func New() (Executor, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	e := &LinuxExecutor{
		watchers: make(map[int]*groupWatch),
		sigchld:  make(chan os.Signal, 1),
		closed:   make(chan struct{}),
		finished: make(chan struct{}),
	}
	notifySIGCHLD(e.sigchld)
	go e.reapLoop()
	return e, nil
}

func (e *LinuxExecutor) Start(cfg model.ProcessConfig, logs LogPaths) (*RunningProcess, error) {
	out, err := os.OpenFile(logs.Stdout, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open stdout log: %w", err)
	}
	defer out.Close()
	errFile, err := os.OpenFile(logs.Stderr, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open stderr log: %w", err)
	}
	defer errFile.Close()

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Cwd
	cmd.Env = mergedEnvironment(cfg.Env)
	cmd.Stdout = out
	cmd.Stderr = errFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Holding this lock from fork through watcher registration prevents the
	// central wait4 loop from reaping a very short-lived child before it is
	// associated with its completion channel.
	e.mu.Lock()
	if err := cmd.Start(); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	pid := cmd.Process.Pid
	done := make(chan ExitResult, 1)
	// The reaper is excluded here, so even an exited child still has an
	// identity in /proc. Read zombie identities too, preserving its exit code.
	stat, statErr := readProcessStat(pid)
	if statErr != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		e.mu.Unlock()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read process identity: %w", statErr)
	}
	startTime := stat.startTime
	e.watchers[pid] = &groupWatch{startTime: startTime, done: done, members: map[int]uint64{pid: startTime}, stopTimeout: cfg.StopTimeout()}
	e.mu.Unlock()
	_ = cmd.Process.Release() // wait4 is owned by reapLoop.
	return &RunningProcess{PID: pid, StartTime: startTime, StartedAt: time.Now(), Done: done}, nil
}

func (e *LinuxExecutor) Adopt(pid int, startTime uint64) (*RunningProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stat, err := readProcessStat(pid)
	if err != nil || stat.startTime != startTime || stat.group != pid {
		return nil, ErrProcessGone
	}
	if watch := e.watchers[pid]; watch != nil {
		return nil, errors.New("process is already tracked")
	}
	done := make(chan ExitResult, 1)
	watch := &groupWatch{startTime: startTime, done: done, adopted: true, members: map[int]uint64{pid: startTime}, stopTimeout: model.DefaultStopTimeout}
	for _, member := range processTable() {
		if member.group == pid {
			watch.members[member.pid] = member.startTime
		}
	}
	e.watchers[pid] = watch
	return &RunningProcess{PID: pid, StartTime: startTime, StartedAt: processStartedAt(startTime), Done: done, Adopted: true}, nil
}

func (e *LinuxExecutor) Alive(pid int, startTime uint64) bool {
	if pid <= 0 || startTime == 0 {
		return false
	}
	stat, err := readProcessStat(pid)
	return err == nil && stat.startTime == startTime && stat.group == pid
}

func (e *LinuxExecutor) SignalGroup(pid int, startTime uint64, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	watch := e.watchers[pid]
	if watch == nil || watch.startTime != startTime {
		return ErrProcessGone
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return signalTrackedGroup(pid, watch, processTable(), sig)
}

func trackedMembers(pid int, watch *groupWatch, table map[int]processStat) []processStat {
	// A matching leader (including our unreaped zombie) or previously seen
	// member anchors ownership. A numeric PGID alone is never sufficient.
	anchored := false
	for memberPID, startTime := range watch.members {
		if stat, ok := table[memberPID]; ok && stat.startTime == startTime && stat.group == pid {
			anchored = true
			break
		}
	}
	if !anchored {
		return nil
	}
	var members []processStat
	for _, stat := range table {
		if stat.group == pid {
			watch.members[stat.pid] = stat.startTime
			if stat.live() {
				members = append(members, stat)
			}
		}
	}
	return members
}

func signalTrackedGroup(pid int, watch *groupWatch, table map[int]processStat, sig syscall.Signal) error {
	members := trackedMembers(pid, watch, table)
	if len(members) == 0 {
		return ErrProcessGone
	}
	if leader, err := readProcessStat(pid); err == nil && leader.startTime == watch.startTime && leader.group == pid {
		return syscall.Kill(-pid, sig)
	}
	// An adopted leader may have been reaped by PID 1. Pin each known member
	// with a pidfd before validating its identity to avoid signalling reused PIDs.
	for _, member := range members {
		fd, err := unix.PidfdOpen(member.pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		actual, statErr := readProcessStat(member.pid)
		if statErr == nil && actual.startTime == member.startTime && actual.group == pid {
			err = unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
		}
		_ = unix.Close(fd)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func (e *LinuxExecutor) Close() error {
	e.once.Do(func() {
		stopSIGCHLD(e.sigchld)
		close(e.closed)
		<-e.finished
		e.mu.Lock()
		for pid, watch := range e.watchers {
			close(watch.done)
			delete(e.watchers, pid)
		}
		e.mu.Unlock()
	})
	return nil
}

func (e *LinuxExecutor) reapLoop() {
	defer close(e.finished)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-e.sigchld:
		case <-ticker.C:
		}
		e.reap()
	}
}

func (e *LinuxExecutor) reap() {
	e.mu.Lock()
	defer e.mu.Unlock()
	table := processTable()
	if table == nil {
		return
	}
	for pid, watch := range e.watchers {
		leader, exists := table[pid]
		members := trackedMembers(pid, watch, table)
		if exists && leader.startTime == watch.startTime && leader.live() {
			watch.emptySeen = false
			continue
		}
		if len(members) > 0 {
			watch.emptySeen = false
			if watch.cleanupAt.IsZero() {
				watch.cleanupAt = time.Now().Add(watch.stopTimeout)
				_ = signalTrackedGroup(pid, watch, table, syscall.SIGTERM)
			} else if !time.Now().Before(watch.cleanupAt) {
				_ = signalTrackedGroup(pid, watch, table, syscall.SIGKILL)
			}
			continue
		}
		// Scan once more after observing the leader exit, since a descendant
		// can be forked between /proc enumeration and reading the leader stat.
		if !watch.emptySeen {
			watch.emptySeen = true
			continue
		}
		result := ExitResult{Code: -1}
		if !watch.adopted {
			// Keep the leader unreaped until cleanup completes; its PID cannot
			// be reused while it anchors the process group's identity.
			var status syscall.WaitStatus
			waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if err != nil || waited != pid {
				continue
			}
			result = exitResult(status)
		}
		watch.done <- result
		close(watch.done)
		delete(e.watchers, pid)
	}
	// Reap exited descendants reparented to the subreaper, without consuming
	// the managed leaders whose wait status belongs to their watcher.
	for pid, stat := range table {
		if stat.parent == os.Getpid() && !stat.live() && e.watchers[pid] == nil {
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		}
	}
}

func exitResult(status syscall.WaitStatus) ExitResult {
	if status.Exited() {
		return ExitResult{Code: status.ExitStatus(), Known: true}
	}
	if status.Signaled() {
		return ExitResult{Code: 128 + int(status.Signal()), Known: true}
	}
	return ExitResult{Code: -1, Known: false}
}

func readProcessStat(pid int) (processStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processStat{}, err
	}
	return parseProcessStat(pid, string(data))
}

func parseProcessStat(pid int, line string) (processStat, error) {
	endComm := strings.LastIndexByte(line, ')')
	if endComm < 0 || endComm+2 >= len(line) {
		return processStat{}, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(line[endComm+2:])
	if len(fields) <= 19 {
		return processStat{}, errors.New("short /proc stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return processStat{}, err
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return processStat{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	return processStat{pid: pid, parent: parent, group: group, startTime: startTime, state: fields[0]}, err
}

func processTable() map[int]processStat {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	table := make(map[int]processStat)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if stat, err := readProcessStat(pid); err == nil {
			table[pid] = stat
		}
	}
	return table
}

var (
	clockTicksOnce sync.Once
	clockTicksHz   uint64 = 100
)

func processStartedAt(startTime uint64) time.Time {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Time{}
	}
	uptimeSeconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}
	}
	ageSeconds := uptimeSeconds - float64(startTime)/float64(linuxClockTicks())
	if ageSeconds < 0 {
		ageSeconds = 0
	}
	return time.Now().Add(-time.Duration(ageSeconds * float64(time.Second)))
}

func linuxClockTicks() uint64 {
	clockTicksOnce.Do(func() {
		data, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			return
		}
		wordSize := strconv.IntSize / 8
		for offset := 0; offset+2*wordSize <= len(data); offset += 2 * wordSize {
			var tag, value uint64
			if wordSize == 8 {
				tag = binary.NativeEndian.Uint64(data[offset : offset+wordSize])
				value = binary.NativeEndian.Uint64(data[offset+wordSize : offset+2*wordSize])
			} else {
				tag = uint64(binary.NativeEndian.Uint32(data[offset : offset+wordSize]))
				value = uint64(binary.NativeEndian.Uint32(data[offset+wordSize : offset+2*wordSize]))
			}
			const atClockTick = 17
			if tag == atClockTick && value > 0 {
				clockTicksHz = value
				return
			}
			if tag == 0 {
				return
			}
		}
	})
	return clockTicksHz
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	order := make([]string, 0)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for key, value := range overrides {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}
