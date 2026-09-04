//go:build linux

package executor

import (
	"bufio"
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
	watchers map[int]chan ExitResult
	sigchld  chan os.Signal
	closed   chan struct{}
	once     sync.Once
}

func New() (Executor, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable child subreaper: %w", err)
	}
	e := &LinuxExecutor{
		watchers: make(map[int]chan ExitResult),
		sigchld:  make(chan os.Signal, 1),
		closed:   make(chan struct{}),
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
	e.watchers[pid] = done
	e.mu.Unlock()

	startTime, statErr := readStartTime(pid)
	if statErr != nil {
		// Fork/exec has already succeeded, so returning without cleanup would
		// leave a live process that the manager never learns about. Kill the
		// newly-created group while its PID is still ours and let the central
		// reaper consume its wait status through the registered watcher.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Process.Release()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil, fmt.Errorf("read process identity: %w", statErr)
	}
	_ = cmd.Process.Release() // wait4 is owned by reapLoop.
	return &RunningProcess{PID: pid, StartTime: startTime, StartedAt: time.Now(), Done: done}, nil
}

func (e *LinuxExecutor) Adopt(pid int, startTime uint64) (*RunningProcess, error) {
	if !e.Alive(pid, startTime) {
		return nil, ErrProcessGone
	}
	done := make(chan ExitResult, 1)
	go func() {
		ticker := time.NewTicker(AdoptPollInterval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-e.closed:
				return
			case <-ticker.C:
				if !e.Alive(pid, startTime) {
					done <- ExitResult{Code: -1, Known: false}
					return
				}
			}
		}
	}()
	return &RunningProcess{PID: pid, StartTime: startTime, StartedAt: processStartedAt(startTime), Done: done, Adopted: true}, nil
}

func (e *LinuxExecutor) Alive(pid int, startTime uint64) bool {
	if pid <= 0 || startTime == 0 {
		return false
	}
	actual, err := readStartTime(pid)
	return err == nil && actual == startTime
}

func (e *LinuxExecutor) SignalGroup(pid int, startTime uint64, force bool) error {
	if !e.Alive(pid, startTime) {
		return ErrProcessGone
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrProcessGone
		}
		return err
	}
	return nil
}

func (e *LinuxExecutor) Close() error {
	e.once.Do(func() {
		stopSIGCHLD(e.sigchld)
		close(e.closed)
	})
	return nil
}

func (e *LinuxExecutor) reapLoop() {
	for {
		select {
		case <-e.closed:
			return
		case <-e.sigchld:
			for {
				e.mu.Lock()
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid > 0 {
					if ch, ok := e.watchers[pid]; ok {
						delete(e.watchers, pid)
						result := exitResult(status)
						select {
						case ch <- result:
						default:
						}
						close(ch)
					}
				}
				e.mu.Unlock()
				if pid <= 0 {
					if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.EINTR) {
						// A later SIGCHLD notification retries. There is no safe
						// action for a transient wait4 failure here.
					}
					break
				}
			}
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

func readStartTime(pid int) (uint64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 4096).ReadString('\n')
	if err != nil && len(line) == 0 {
		return 0, err
	}
	endComm := strings.LastIndexByte(line, ')')
	if endComm < 0 || endComm+2 >= len(line) {
		return 0, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(line[endComm+2:])
	// fields[0] is stat field 3 (state); starttime is field 22. A zombie
	// still has a /proc entry but is not a live process that can be adopted.
	if len(fields) <= 19 {
		return 0, errors.New("short /proc stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, os.ErrNotExist
	}
	return strconv.ParseUint(fields[19], 10, 64)
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
