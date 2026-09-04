//go:build !linux

package executor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"akid/internal/model"
)

// localExecutor exists so storage/protocol tests and CLI builds work on
// non-Linux hosts. It deliberately does not pretend to provide Linux process
// group or adoption semantics.
type localExecutor struct {
	mu     sync.Mutex
	procs  map[int]localProcess
	nextID atomic.Uint64
}

type localProcess struct {
	process *os.Process
	token   uint64
}

func New() (Executor, error) {
	return &localExecutor{procs: make(map[int]localProcess)}, nil
}

func (e *localExecutor) Start(cfg model.ProcessConfig, logs LogPaths) (*RunningProcess, error) {
	out, err := os.OpenFile(logs.Stdout, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errFile, err := os.OpenFile(logs.Stderr, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer errFile.Close()
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = cfg.Cwd, out, errFile
	cmd.Env = mergeEnvOther(cfg.Env)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	token := e.nextID.Add(1)
	done := make(chan ExitResult, 1)
	e.mu.Lock()
	e.procs[cmd.Process.Pid] = localProcess{process: cmd.Process, token: token}
	e.mu.Unlock()
	go func() {
		err := cmd.Wait()
		result := ExitResult{Code: -1, Known: false}
		if cmd.ProcessState != nil {
			result.Code, result.Known = cmd.ProcessState.ExitCode(), true
		} else if err == nil {
			result.Code, result.Known = 0, true
		}
		e.mu.Lock()
		delete(e.procs, cmd.Process.Pid)
		e.mu.Unlock()
		done <- result
		close(done)
	}()
	return &RunningProcess{PID: cmd.Process.Pid, StartTime: token, StartedAt: time.Now(), Done: done}, nil
}

func (e *localExecutor) Adopt(int, uint64) (*RunningProcess, error) { return nil, ErrProcessGone }
func (e *localExecutor) Alive(pid int, token uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.procs[pid]
	return ok && p.token == token
}
func (e *localExecutor) SignalGroup(pid int, token uint64, _ bool) error {
	e.mu.Lock()
	p, ok := e.procs[pid]
	e.mu.Unlock()
	if !ok || p.token != token {
		return ErrProcessGone
	}
	if err := p.process.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}
	return nil
}
func (e *localExecutor) Close() error { return nil }

func mergeEnvOther(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[strings.ToUpper(key)] = key + "=" + value
		}
	}
	for key, value := range overrides {
		env[strings.ToUpper(key)] = key + "=" + value
	}
	result := make([]string, 0, len(env))
	for _, item := range env {
		result = append(result, item)
	}
	return result
}
