package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

type ProcessStatus string

const (
	StatusStopped    ProcessStatus = "stopped"
	StatusStarting   ProcessStatus = "starting"
	StatusRunning    ProcessStatus = "running"
	StatusStopping   ProcessStatus = "stopping"
	StatusExited     ProcessStatus = "exited"
	StatusRestarting ProcessStatus = "restarting"
)

type LogStream string

const (
	LogStdout LogStream = "stdout"
	LogStderr LogStream = "stderr"
)

const DefaultStopTimeout = 5 * time.Second

type ProcessConfig struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Command       string            `json:"command"`
	Args          []string          `json:"args,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Restart       RestartPolicy     `json:"restart"`
	StopTimeoutNS int64             `json:"stop_timeout_ns,omitempty"`
}

func (c ProcessConfig) StopTimeout() time.Duration {
	if c.StopTimeoutNS <= 0 {
		return DefaultStopTimeout
	}
	return time.Duration(c.StopTimeoutNS)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (c *ProcessConfig) NormalizeAndValidate() error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if !validName.MatchString(c.Name) {
		return errors.New("name must contain only letters, numbers, '.', '_' or '-' and cannot start with punctuation")
	}
	if c.Command == "" {
		return errors.New("command is required")
	}
	if c.Restart == "" {
		c.Restart = RestartAlways
	}
	switch c.Restart {
	case RestartAlways, RestartOnFailure, RestartNever:
	default:
		return fmt.Errorf("invalid restart policy %q", c.Restart)
	}
	if c.StopTimeoutNS < 0 {
		return errors.New("stop timeout cannot be negative")
	}
	if c.Args == nil {
		c.Args = []string{}
	}
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	return nil
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type RuntimeHint struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
}

type PersistedProcess struct {
	ProcessConfig
	Desired DesiredState `json:"desired"`
	Hint    *RuntimeHint `json:"hint,omitempty"`
}

type PersistedState struct {
	Version   int                `json:"version"`
	Processes []PersistedProcess `json:"processes"`
}

type RuntimeState struct {
	PID          int           `json:"pid,omitempty"`
	StartTime    uint64        `json:"start_time,omitempty"`
	Status       ProcessStatus `json:"status"`
	StartedAt    time.Time     `json:"started_at,omitempty"`
	StoppedAt    time.Time     `json:"stopped_at,omitempty"`
	ExitCode     *int          `json:"exit_code,omitempty"`
	RestartCount uint64        `json:"restart_count"`
	NextRetryAt  time.Time     `json:"next_retry_at,omitempty"`
}

type ProcessInfo struct {
	Epoch         string        `json:"epoch,omitempty"`
	Revision      uint64        `json:"revision,omitempty"`
	Config        ProcessConfig `json:"config"`
	Desired       DesiredState  `json:"desired"`
	Runtime       RuntimeState  `json:"runtime"`
	OutGeneration uint64        `json:"out_generation"`
	ErrGeneration uint64        `json:"err_generation"`
}

type ProcessMetrics struct {
	ID           string  `json:"id"`
	PID          int     `json:"pid,omitempty"`
	CPUPercent   float64 `json:"cpu_percent,omitempty"`
	MemoryBytes  uint64  `json:"memory_bytes,omitempty"`
	CPUAvailable bool    `json:"cpu_available"`
	Available    bool    `json:"available"`
}

type Event struct {
	Name     string           `json:"event"`
	Data     ProcessInfo      `json:"data"`
	Snapshot *ProcessSnapshot `json:"snapshot,omitempty"`
}

type ProcessSnapshot struct {
	Epoch     string        `json:"epoch"`
	Revision  uint64        `json:"revision"`
	Processes []ProcessInfo `json:"processes"`
}
