package executor

import (
	"errors"
	"time"

	"akid/internal/model"
)

var ErrProcessGone = errors.New("process is no longer alive")

type LogPaths struct {
	Stdout string
	Stderr string
}

type ExitResult struct {
	Code  int
	Known bool
}

type RunningProcess struct {
	PID       int
	StartTime uint64
	StartedAt time.Time
	Done      <-chan ExitResult
	Adopted   bool
}

type Executor interface {
	Start(model.ProcessConfig, LogPaths) (*RunningProcess, error)
	Adopt(pid int, startTime uint64) (*RunningProcess, error)
	Alive(pid int, startTime uint64) bool
	SignalGroup(pid int, startTime uint64, force bool) error
	Close() error
}

// Adopt polling is intentionally part of the executor contract. A newly
// started daemon cannot waitpid a process orphaned by the previous daemon.
const AdoptPollInterval = 500 * time.Millisecond
