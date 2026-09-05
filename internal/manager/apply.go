package manager

import (
	"context"
	"errors"
	"maps"
	"slices"

	"akid/internal/model"
)

type ApplyEntry struct {
	Action  string            `json:"action"`
	Process model.ProcessInfo `json:"process"`
	Error   *ApplyError       `json:"error,omitempty"`
}

type ApplyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ApplyResult struct {
	Processes []ApplyEntry `json:"processes"`
}

type applyCmd struct {
	configs []model.ProcessConfig
	reply   chan opResult
}

func (m *Manager) Apply(ctx context.Context, configs []model.ProcessConfig) (ApplyResult, error) {
	cloned := make([]model.ProcessConfig, len(configs))
	for i, cfg := range configs {
		cloned[i] = cloneConfig(cfg)
	}
	reply := make(chan opResult, 1)
	if err := m.send(ctx, applyCmd{configs: cloned, reply: reply}); err != nil {
		return ApplyResult{}, err
	}
	return await[ApplyResult](ctx, reply)
}

// handleApply validates the entire batch and persists its configuration/intent
// before starting or stopping anything. Runtime failures are reported per entry:
// process side effects cannot be rolled back as a filesystem transaction.
func (m *Manager) handleApply(cmd applyCmd) {
	if m.shuttingDown {
		cmd.reply <- opResult{err: coded(CodeInvalidState, "daemon is shutting down")}
		return
	}
	seen := make(map[string]bool)
	for i := range cmd.configs {
		cfg := &cmd.configs[i]
		if cfg.ID != "" {
			cmd.reply <- opResult{err: coded(CodeInvalidConfig, "apply identifies processes by name; id must be omitted")}
			return
		}
		if err := cfg.NormalizeAndValidate(); err != nil {
			cmd.reply <- opResult{err: coded(CodeInvalidConfig, "process %q: %v", cfg.Name, err)}
			return
		}
		if seen[cfg.Name] {
			cmd.reply <- opResult{err: coded(CodeNameConflict, "duplicate process name %q", cfg.Name)}
			return
		}
		seen[cfg.Name] = true
		if id, exists := m.names[cfg.Name]; exists {
			if m.records[id].deleting {
				cmd.reply <- opResult{err: coded(CodeInvalidState, "process %q is being deleted", cfg.Name)}
				return
			}
			cfg.ID = id
		} else {
			id, err := model.NewID()
			if err != nil {
				cmd.reply <- opResult{err: coded(CodeInternal, "generate process id: %v", err)}
				return
			}
			cfg.ID = id
		}
	}

	type previous struct {
		r            *record
		config       model.ProcessConfig
		desired      model.DesiredState
		restartAfter bool
	}
	var changed []previous
	var created []*record
	actions := make([]string, len(cmd.configs))
	rollback := func() {
		for _, old := range changed {
			old.r.config, old.r.desired = old.config, old.desired
			old.r.restartAfter = old.restartAfter
		}
		for _, r := range created {
			delete(m.records, r.config.ID)
			delete(m.names, r.config.Name)
			_ = m.logs.Remove(r.config.Name, r.config.ID, false)
		}
	}
	for i, cfg := range cmd.configs {
		r := m.records[cfg.ID]
		if r != nil {
			if equalConfig(r.config, cfg) {
				actions[i] = "unchanged"
				continue
			}
			changed = append(changed, previous{r: r, config: r.config, desired: r.desired, restartAfter: r.restartAfter})
			r.config, r.desired = cfg, model.DesiredRunning
			r.restartAfter = r.proc != nil
			actions[i] = "updated"
			continue
		}
		if err := m.logs.Register(cfg.Name, cfg.ID); err != nil {
			rollback()
			cmd.reply <- opResult{err: coded(CodeInternal, "prepare logs for %q: %v", cfg.Name, err)}
			return
		}
		r = &record{config: cfg, desired: model.DesiredRunning, runtime: model.RuntimeState{Status: model.StatusStopped}}
		m.records[cfg.ID], m.names[cfg.Name] = r, cfg.ID
		created = append(created, r)
		actions[i] = "created"
	}
	if len(changed)+len(created) > 0 {
		if err := m.persist(); err != nil {
			rollback()
			cmd.reply <- opResult{err: coded(CodeInternal, "save applied configuration: %v", err)}
			return
		}
	}
	result := ApplyResult{Processes: make([]ApplyEntry, 0, len(cmd.configs))}
	for i, cfg := range cmd.configs {
		r := m.records[cfg.ID]
		var err error
		switch actions[i] {
		case "created":
			err = m.start(r)
		case "updated":
			err = m.restart(r)
		}
		if actions[i] != "unchanged" {
			m.emit("process.updated", r)
		}
		entry := ApplyEntry{Action: actions[i], Process: m.info(r)}
		if err != nil {
			entry.Error = &ApplyError{Code: CodeInternal, Message: err.Error()}
			var managed *Error
			if errors.As(err, &managed) {
				entry.Error.Code = managed.Code
			}
		}
		result.Processes = append(result.Processes, entry)
	}
	cmd.reply <- opResult{value: result}
}

func equalConfig(a, b model.ProcessConfig) bool {
	return a.Name == b.Name && a.Command == b.Command && a.Cwd == b.Cwd &&
		a.Restart == b.Restart && a.StopTimeout() == b.StopTimeout() &&
		slices.Equal(a.Args, b.Args) && maps.Equal(a.Env, b.Env)
}
