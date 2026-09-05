package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"akid/internal/executor"
	"akid/internal/model"
	"akid/internal/storage"
)

const (
	CodeNotFound      = "PROCESS_NOT_FOUND"
	CodeNameConflict  = "PROCESS_NAME_CONFLICT"
	CodeInvalidConfig = "INVALID_CONFIG"
	CodeSpawnFailed   = "SPAWN_FAILED"
	CodeInvalidState  = "INVALID_STATE"
	CodeInternal      = "INTERNAL_ERROR"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func coded(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type LogRegistry interface {
	Register(name string, owner ...string) error
	Remove(name, owner string, purge bool) error
	Paths(name string) (stdout, stderr string)
	Generation(name string, stream model.LogStream) uint64
}

type Manager struct {
	store  storage.Store
	exec   executor.Executor
	logs   LogRegistry
	logger *log.Logger

	events   chan any
	done     chan struct{}
	lifeMu   sync.RWMutex
	closed   bool
	records  map[string]*record
	names    map[string]string
	subs     map[uint64]chan model.Event
	nextSub  atomic.Uint64
	epoch    string
	revision uint64

	shuttingDown bool
	terminated   bool
	shutdownWait []chan opResult
}

type record struct {
	revision uint64
	config   model.ProcessConfig
	desired  model.DesiredState
	runtime  model.RuntimeState
	hint     *model.RuntimeHint
	proc     *executor.RunningProcess

	startedMono  time.Time
	crashStreak  uint
	timer        *time.Timer
	timerGen     uint64
	stopTimer    *time.Timer
	restartAfter bool
	deleting     bool
	deleteWait   []deleteWaiter
}

type deleteWaiter struct {
	purge bool
	ch    chan opResult
}

type DeleteResult struct {
	Name  string `json:"name"`
	Purge bool   `json:"purge"`
}

type opResult struct {
	value any
	err   error
}

type restoreCmd struct{ reply chan opResult }
type createCmd struct {
	config model.ProcessConfig
	reply  chan opResult
}
type actionCmd struct {
	kind  string
	id    string
	reply chan opResult
}
type deleteCmd struct {
	id    string
	purge bool
	reply chan opResult
}
type listCmd struct{ reply chan opResult }
type getCmd struct {
	id    string
	reply chan opResult
}
type exitEvent struct {
	id        string
	pid       int
	startTime uint64
	result    executor.ExitResult
}
type backoffEvent struct {
	id  string
	gen uint64
}
type stopTimeoutEvent struct {
	id        string
	pid       int
	startTime uint64
}
type subscribeCmd struct {
	ch    chan model.Event
	reply chan opResult
}
type unsubscribeEvent struct{ id uint64 }
type shutdownCmd struct{ reply chan opResult }

func New(store storage.Store, exec executor.Executor, logs LogRegistry, logger *log.Logger) (*Manager, error) {
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	epoch, err := model.NewID()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		epoch:   epoch,
		store:   store,
		exec:    exec,
		logs:    logs,
		logger:  logger,
		events:  make(chan any, 256),
		done:    make(chan struct{}),
		records: make(map[string]*record),
		names:   make(map[string]string),
		subs:    make(map[uint64]chan model.Event),
	}
	for _, persisted := range state.Processes {
		cfg := cloneConfig(persisted.ProcessConfig)
		if err := logs.Register(cfg.Name, cfg.ID); err != nil {
			return nil, fmt.Errorf("prepare logs for %s: %w", cfg.Name, err)
		}
		status := model.StatusStopped
		if persisted.Desired == model.DesiredRunning {
			status = model.StatusExited
		}
		r := &record{
			config:  cfg,
			desired: persisted.Desired,
			runtime: model.RuntimeState{Status: status},
			hint:    cloneHint(persisted.Hint),
		}
		m.records[cfg.ID] = r
		m.names[cfg.Name] = cfg.ID
	}
	go m.loop()
	reply := make(chan opResult, 1)
	m.events <- restoreCmd{reply: reply}
	result := <-reply
	if result.err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = m.Shutdown(cleanupCtx)
		cancel()
		return nil, result.err
	}
	return m, nil
}

func (m *Manager) Create(ctx context.Context, cfg model.ProcessConfig) (model.ProcessInfo, error) {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, createCmd{config: cloneConfig(cfg), reply: reply}); err != nil {
		return model.ProcessInfo{}, err
	}
	value, err := await[model.ProcessInfo](ctx, reply)
	return value, err
}

func (m *Manager) Start(ctx context.Context, id string) (model.ProcessInfo, error) {
	return m.action(ctx, "start", id)
}

func (m *Manager) Stop(ctx context.Context, id string) (model.ProcessInfo, error) {
	return m.action(ctx, "stop", id)
}

func (m *Manager) Restart(ctx context.Context, id string) (model.ProcessInfo, error) {
	return m.action(ctx, "restart", id)
}

func (m *Manager) action(ctx context.Context, kind, id string) (model.ProcessInfo, error) {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, actionCmd{kind: kind, id: id, reply: reply}); err != nil {
		return model.ProcessInfo{}, err
	}
	return await[model.ProcessInfo](ctx, reply)
}

func (m *Manager) Delete(ctx context.Context, id string, purge bool) (DeleteResult, error) {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, deleteCmd{id: id, purge: purge, reply: reply}); err != nil {
		return DeleteResult{}, err
	}
	return await[DeleteResult](ctx, reply)
}

func (m *Manager) List(ctx context.Context) ([]model.ProcessInfo, error) {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, listCmd{reply: reply}); err != nil {
		return nil, err
	}
	return await[[]model.ProcessInfo](ctx, reply)
}

func (m *Manager) Get(ctx context.Context, id string) (model.ProcessInfo, error) {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, getCmd{id: id, reply: reply}); err != nil {
		return model.ProcessInfo{}, err
	}
	return await[model.ProcessInfo](ctx, reply)
}

func (m *Manager) Subscribe(ctx context.Context) (<-chan model.Event, error) {
	ch := make(chan model.Event, 1024)
	reply := make(chan opResult, 1)
	if err := m.send(ctx, subscribeCmd{ch: ch, reply: reply}); err != nil {
		return nil, err
	}
	id, err := await[uint64](ctx, reply)
	if err != nil {
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			m.post(unsubscribeEvent{id: id})
		case <-m.done:
		}
	}()
	return ch, nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	reply := make(chan opResult, 1)
	if err := m.send(ctx, shutdownCmd{reply: reply}); err != nil {
		return err
	}
	_, err := await[struct{}](ctx, reply)
	return err
}

func (m *Manager) send(ctx context.Context, event any) error {
	for {
		m.lifeMu.RLock()
		if m.closed {
			m.lifeMu.RUnlock()
			return errors.New("process manager is closed")
		}
		select {
		case m.events <- event:
			m.lifeMu.RUnlock()
			return nil
		default:
			m.lifeMu.RUnlock()
		}
		select {
		case <-m.done:
			return errors.New("process manager is closed")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func await[T any](ctx context.Context, reply <-chan opResult) (T, error) {
	var zero T
	select {
	case result := <-reply:
		if result.err != nil {
			return zero, result.err
		}
		value, ok := result.value.(T)
		if !ok {
			return zero, errors.New("internal response type mismatch")
		}
		return value, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (m *Manager) loop() {
	for event := range m.events {
		switch e := event.(type) {
		case restoreCmd:
			e.reply <- opResult{err: m.restore()}
		case createCmd:
			m.handleCreate(e)
		case actionCmd:
			m.handleAction(e)
		case deleteCmd:
			m.handleDelete(e)
		case listCmd:
			m.handleList(e)
		case getCmd:
			m.handleGet(e)
		case exitEvent:
			m.handleExit(e)
		case backoffEvent:
			m.handleBackoff(e)
		case stopTimeoutEvent:
			m.handleStopTimeout(e)
		case subscribeCmd:
			id := m.nextSub.Add(1)
			m.subs[id] = e.ch
			e.ch <- model.Event{Name: "process.snapshot", Snapshot: &model.ProcessSnapshot{Epoch: m.epoch, Revision: m.revision, Processes: m.list()}}
			e.reply <- opResult{value: id}
		case unsubscribeEvent:
			if ch, ok := m.subs[e.id]; ok {
				delete(m.subs, e.id)
				close(ch)
			}
		case shutdownCmd:
			m.handleShutdown(e)
		}
		if m.terminated {
			for id, ch := range m.subs {
				close(ch)
				delete(m.subs, id)
			}
			m.rejectPending()
			m.lifeMu.Lock()
			close(m.done)
			m.lifeMu.Unlock()
			return
		}
	}
}

func (m *Manager) restore() error {
	changed := false
	for _, r := range m.records {
		if r.hint != nil && m.exec.Alive(r.hint.PID, r.hint.StartTime) {
			proc, err := m.exec.Adopt(r.hint.PID, r.hint.StartTime)
			if err == nil {
				r.proc = proc
				r.runtime.PID = proc.PID
				r.runtime.StartTime = proc.StartTime
				r.runtime.Status = model.StatusRunning
				startedAt := proc.StartedAt
				if startedAt.IsZero() {
					startedAt = time.Now()
				}
				r.runtime.StartedAt = startedAt
				r.startedMono = startedAt
				m.watch(r.config.ID, proc)
				if r.desired == model.DesiredStopped {
					m.beginStop(r)
				} else {
					m.emit("process.started", r)
				}
				continue
			}
		}
		r.hint = nil
		changed = true
		if r.desired == model.DesiredStopped {
			continue
		}
		if err := m.spawn(r, false); err != nil {
			if !r.runtime.NextRetryAt.IsZero() {
				m.logf("restore %s: %v; retry scheduled at %s", r.config.Name, err, r.runtime.NextRetryAt.Format(time.RFC3339))
			} else {
				m.logf("restore %s: %v; no retry scheduled", r.config.Name, err)
			}
		}
	}
	if changed {
		return m.persist()
	}
	return nil
}

func (m *Manager) handleCreate(cmd createCmd) {
	cfg := cloneConfig(cmd.config)
	if m.shuttingDown {
		cmd.reply <- opResult{err: coded(CodeInvalidState, "daemon is shutting down")}
		return
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		cmd.reply <- opResult{err: coded(CodeInvalidConfig, "%v", err)}
		return
	}
	if _, exists := m.names[cfg.Name]; exists {
		cmd.reply <- opResult{err: coded(CodeNameConflict, "process %q already exists", cfg.Name)}
		return
	}
	if cfg.ID == "" {
		var err error
		cfg.ID, err = model.NewID()
		if err != nil {
			cmd.reply <- opResult{err: coded(CodeInternal, "generate process id: %v", err)}
			return
		}
	}
	if _, exists := m.records[cfg.ID]; exists {
		cmd.reply <- opResult{err: coded(CodeInvalidConfig, "process id %q already exists", cfg.ID)}
		return
	}
	if err := m.logs.Register(cfg.Name, cfg.ID); err != nil {
		cmd.reply <- opResult{err: coded(CodeInternal, "prepare logs: %v", err)}
		return
	}
	r := &record{config: cfg, desired: model.DesiredStopped, runtime: model.RuntimeState{Status: model.StatusStopped}}
	m.records[cfg.ID] = r
	m.names[cfg.Name] = cfg.ID
	if err := m.persist(); err != nil {
		delete(m.records, cfg.ID)
		delete(m.names, cfg.Name)
		_ = m.logs.Remove(cfg.Name, cfg.ID, false)
		cmd.reply <- opResult{err: coded(CodeInternal, "save state: %v", err)}
		return
	}
	m.emit("process.updated", r)
	cmd.reply <- opResult{value: m.info(r)}
}

func (m *Manager) handleAction(cmd actionCmd) {
	r := m.find(cmd.id)
	if r == nil {
		cmd.reply <- opResult{err: coded(CodeNotFound, "process %q not found", cmd.id)}
		return
	}
	if m.shuttingDown {
		cmd.reply <- opResult{err: coded(CodeInvalidState, "daemon is shutting down")}
		return
	}
	if r.deleting {
		cmd.reply <- opResult{err: coded(CodeInvalidState, "process %q is being deleted", r.config.Name)}
		return
	}
	var err error
	switch cmd.kind {
	case "start":
		err = m.start(r)
	case "stop":
		err = m.stop(r)
	case "restart":
		err = m.restart(r)
	default:
		err = coded(CodeInternal, "unknown action %q", cmd.kind)
	}
	if err != nil {
		cmd.reply <- opResult{err: err}
		return
	}
	m.emit("process.updated", r)
	cmd.reply <- opResult{value: m.info(r)}
}

func (m *Manager) start(r *record) error {
	r.desired = model.DesiredRunning
	r.deleting = false
	switch r.runtime.Status {
	case model.StatusRunning, model.StatusStarting, model.StatusRestarting:
		return m.persist()
	case model.StatusStopping:
		r.restartAfter = true
		return m.persist()
	case model.StatusStopped, model.StatusExited:
		m.cancelBackoff(r)
		return m.spawn(r, false)
	default:
		return coded(CodeInvalidState, "cannot start process in state %q", r.runtime.Status)
	}
}

func (m *Manager) stop(r *record) error {
	r.desired = model.DesiredStopped
	r.restartAfter = false
	m.cancelBackoff(r)
	switch r.runtime.Status {
	case model.StatusStopped:
		return m.persist()
	case model.StatusExited:
		r.runtime.Status = model.StatusStopped
		r.runtime.NextRetryAt = time.Time{}
		r.hint = nil
		r.runtime.StoppedAt = time.Now()
		m.emit("process.stopped", r)
		return m.persist()
	case model.StatusStopping:
		return m.persist()
	case model.StatusRunning, model.StatusStarting, model.StatusRestarting:
		m.beginStop(r)
		return m.persist()
	default:
		return coded(CodeInvalidState, "cannot stop process in state %q", r.runtime.Status)
	}
}

func (m *Manager) restart(r *record) error {
	r.desired = model.DesiredRunning
	m.cancelBackoff(r)
	switch r.runtime.Status {
	case model.StatusRunning, model.StatusStarting, model.StatusRestarting:
		r.restartAfter = true
		m.beginStop(r)
		return m.persist()
	case model.StatusStopping:
		r.restartAfter = true
		return m.persist()
	case model.StatusStopped, model.StatusExited:
		return m.spawn(r, false)
	default:
		return coded(CodeInvalidState, "cannot restart process in state %q", r.runtime.Status)
	}
}

func (m *Manager) beginStop(r *record) {
	if r.runtime.Status == model.StatusStopping {
		return
	}
	r.runtime.Status = model.StatusStopping
	m.emit("process.updated", r)
	if r.proc == nil {
		m.post(exitEvent{id: r.config.ID, pid: r.runtime.PID, startTime: r.runtime.StartTime, result: executor.ExitResult{Code: -1}})
		return
	}
	pid, startTime := r.proc.PID, r.proc.StartTime
	if err := m.exec.SignalGroup(pid, startTime, false); err != nil {
		if errors.Is(err, executor.ErrProcessGone) {
			m.post(exitEvent{id: r.config.ID, pid: pid, startTime: startTime, result: executor.ExitResult{Code: -1}})
		} else {
			m.logf("stop %s: %v", r.config.Name, err)
		}
	}
	if r.stopTimer != nil {
		r.stopTimer.Stop()
	}
	r.stopTimer = time.AfterFunc(r.config.StopTimeout(), func() {
		m.post(stopTimeoutEvent{id: r.config.ID, pid: pid, startTime: startTime})
	})
}

func (m *Manager) handleStopTimeout(event stopTimeoutEvent) {
	r := m.records[event.id]
	if r == nil || r.runtime.Status != model.StatusStopping || r.proc == nil || r.proc.PID != event.pid || r.proc.StartTime != event.startTime {
		return
	}
	if err := m.exec.SignalGroup(event.pid, event.startTime, true); err != nil && !errors.Is(err, executor.ErrProcessGone) {
		m.logf("force stop %s: %v", r.config.Name, err)
	}
}

func (m *Manager) spawn(r *record, automatic bool) error {
	m.cancelBackoff(r)
	r.runtime.Status = model.StatusStarting
	r.runtime.NextRetryAt = time.Time{}
	m.emit("process.updated", r)
	stdout, stderr := m.logs.Paths(r.config.Name)
	proc, err := m.exec.Start(r.config, executor.LogPaths{Stdout: stdout, Stderr: stderr})
	if err != nil {
		r.proc = nil
		r.hint = nil
		r.runtime.PID = 0
		r.runtime.StartTime = 0
		r.runtime.Status = model.StatusExited
		if m.shouldRestart(r, 1, true) && !m.shuttingDown {
			m.scheduleBackoff(r)
		} else {
			r.desired = model.DesiredStopped
			m.emit("process.exited", r)
		}
		_ = m.persistAsync("spawn failure")
		return coded(CodeSpawnFailed, "start %q: %v", r.config.Name, err)
	}
	now := time.Now()
	startedAt := proc.StartedAt
	if startedAt.IsZero() {
		startedAt = now
	}
	r.proc = proc
	r.startedMono = startedAt
	r.runtime.PID = proc.PID
	r.runtime.StartTime = proc.StartTime
	r.runtime.Status = model.StatusRunning
	r.runtime.StartedAt = startedAt
	r.runtime.StoppedAt = time.Time{}
	r.runtime.ExitCode = nil
	if automatic {
		r.runtime.RestartCount++
	}
	if proc.StartTime != 0 {
		r.hint = &model.RuntimeHint{PID: proc.PID, StartTime: proc.StartTime}
	} else {
		r.hint = nil
	}
	m.watch(r.config.ID, proc)
	persistErr := m.persist()
	m.emit("process.started", r)
	if persistErr != nil {
		return coded(CodeInternal, "save state after starting %q: %v", r.config.Name, persistErr)
	}
	return nil
}

func (m *Manager) watch(id string, proc *executor.RunningProcess) {
	go func() {
		result, ok := <-proc.Done
		if !ok {
			return
		}
		m.post(exitEvent{id: id, pid: proc.PID, startTime: proc.StartTime, result: result})
	}()
}

func (m *Manager) handleExit(event exitEvent) {
	r := m.records[event.id]
	if r == nil || r.proc == nil || r.proc.PID != event.pid || r.proc.StartTime != event.startTime {
		return
	}
	wasStopping := r.runtime.Status == model.StatusStopping
	if r.stopTimer != nil {
		r.stopTimer.Stop()
		r.stopTimer = nil
	}
	r.proc = nil
	r.hint = nil
	r.runtime.PID = 0
	r.runtime.StartTime = 0
	r.runtime.NextRetryAt = time.Time{}
	r.runtime.StoppedAt = time.Now()
	if event.result.Known {
		code := event.result.Code
		r.runtime.ExitCode = &code
	} else {
		r.runtime.ExitCode = nil
	}

	if r.deleting {
		r.runtime.Status = model.StatusStopped
		m.finishDelete(r)
		m.checkShutdownComplete()
		return
	}

	if m.shuttingDown {
		r.runtime.Status = model.StatusStopped
		_ = m.persistAsync("shutdown exit")
		m.emit("process.stopped", r)
		m.checkShutdownComplete()
		return
	}
	if wasStopping {
		if r.restartAfter {
			r.restartAfter = false
			r.runtime.Status = model.StatusRestarting
			m.emit("process.restarting", r)
			if err := m.spawn(r, false); err != nil {
				m.logf("restart %s: %v", r.config.Name, err)
			}
			return
		}
		r.runtime.Status = model.StatusStopped
		r.desired = model.DesiredStopped
		_ = m.persistAsync("stop")
		m.emit("process.stopped", r)
		return
	}

	r.runtime.Status = model.StatusExited
	if time.Since(r.startedMono) >= 60*time.Second {
		r.crashStreak = 0
	}
	willRestart := m.shouldRestart(r, event.result.Code, !event.result.Known)
	if !willRestart {
		r.desired = model.DesiredStopped
	}
	m.emit("process.exited", r)
	if willRestart {
		m.scheduleBackoff(r)
	} else {
		_ = m.persistAsync("terminal exit")
	}
}

func (m *Manager) shouldRestart(r *record, exitCode int, unknown bool) bool {
	if r.desired != model.DesiredRunning {
		return false
	}
	switch r.config.Restart {
	case model.RestartAlways:
		return true
	case model.RestartOnFailure:
		return unknown || exitCode != 0
	default:
		return false
	}
}

func (m *Manager) scheduleBackoff(r *record) {
	m.cancelBackoff(r)
	r.crashStreak++
	delay := time.Second << min(r.crashStreak-1, 5)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	r.runtime.Status = model.StatusExited
	r.runtime.NextRetryAt = time.Now().Add(delay)
	r.timerGen++
	gen := r.timerGen
	r.timer = time.AfterFunc(delay, func() { m.post(backoffEvent{id: r.config.ID, gen: gen}) })
	_ = m.persistAsync("backoff")
	m.emit("process.backoff", r)
}

func (m *Manager) handleBackoff(event backoffEvent) {
	r := m.records[event.id]
	if r == nil || r.timerGen != event.gen || r.desired != model.DesiredRunning || r.runtime.Status != model.StatusExited || m.shuttingDown {
		return
	}
	r.timer = nil
	r.runtime.Status = model.StatusRestarting
	r.runtime.NextRetryAt = time.Time{}
	m.emit("process.restarting", r)
	if err := m.spawn(r, true); err != nil {
		m.logf("automatic restart %s: %v", r.config.Name, err)
	}
}

func (m *Manager) cancelBackoff(r *record) {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.timerGen++
	r.runtime.NextRetryAt = time.Time{}
}

func (m *Manager) handleDelete(cmd deleteCmd) {
	r := m.find(cmd.id)
	if r == nil {
		cmd.reply <- opResult{err: coded(CodeNotFound, "process %q not found", cmd.id)}
		return
	}
	if m.shuttingDown {
		cmd.reply <- opResult{err: coded(CodeInvalidState, "daemon is shutting down")}
		return
	}
	if r.runtime.Status == model.StatusStopped || r.runtime.Status == model.StatusExited {
		r.deleteWait = append(r.deleteWait, deleteWaiter{purge: cmd.purge, ch: cmd.reply})
		m.finishDelete(r)
		return
	}
	r.deleting = true
	r.deleteWait = append(r.deleteWait, deleteWaiter{purge: cmd.purge, ch: cmd.reply})
	r.desired = model.DesiredStopped
	r.restartAfter = false
	m.cancelBackoff(r)
	m.beginStop(r)
	_ = m.persistAsync("delete stop")
}

func (m *Manager) finishDelete(r *record) {
	waiters := r.deleteWait
	r.deleteWait = nil
	purge := false
	for _, waiter := range waiters {
		purge = purge || waiter.purge
	}
	delete(m.records, r.config.ID)
	if err := m.persist(); err != nil {
		m.records[r.config.ID] = r
		r.deleting = false
		m.emit("process.updated", r)
		for _, waiter := range waiters {
			waiter.ch <- opResult{err: coded(CodeInternal, "save state: %v", err)}
		}
		return
	}
	m.cancelBackoff(r)
	// The state owner retains the name until identity-checked log cleanup
	// finishes. A concurrent create cannot acquire the name in between.
	err := m.logs.Remove(r.config.Name, r.config.ID, purge)
	if err != nil {
		m.records[r.config.ID] = r
		r.deleting = false
		r.desired = model.DesiredStopped
		r.runtime.Status = model.StatusStopped
		_ = m.persistAsync("failed log cleanup")
		m.emit("process.updated", r)
		for _, waiter := range waiters {
			waiter.ch <- opResult{err: coded(CodeInternal, "remove logs: %v", err)}
		}
		return
	}
	delete(m.names, r.config.Name)
	m.emit("process.deleted", r)
	for _, waiter := range waiters {
		waiter.ch <- opResult{value: DeleteResult{Name: r.config.Name, Purge: waiter.purge}}
	}
}

func (m *Manager) handleList(cmd listCmd) {
	cmd.reply <- opResult{value: m.list()}
}

func (m *Manager) list() []model.ProcessInfo {
	list := make([]model.ProcessInfo, 0, len(m.records))
	for _, r := range m.records {
		list = append(list, m.info(r))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Config.Name < list[j].Config.Name })
	return list
}

func (m *Manager) handleGet(cmd getCmd) {
	r := m.find(cmd.id)
	if r == nil {
		cmd.reply <- opResult{err: coded(CodeNotFound, "process %q not found", cmd.id)}
		return
	}
	cmd.reply <- opResult{value: m.info(r)}
}

func (m *Manager) handleShutdown(cmd shutdownCmd) {
	m.lifeMu.Lock()
	m.closed = true
	m.lifeMu.Unlock()
	m.shuttingDown = true
	m.shutdownWait = append(m.shutdownWait, cmd.reply)
	for _, r := range m.records {
		m.cancelBackoff(r)
		r.restartAfter = false
		switch r.runtime.Status {
		case model.StatusRunning, model.StatusStarting, model.StatusRestarting:
			m.beginStop(r)
		case model.StatusExited:
			r.runtime.Status = model.StatusStopped
		}
	}
	_ = m.persistAsync("shutdown")
	m.checkShutdownComplete()
}

func (m *Manager) checkShutdownComplete() {
	if !m.shuttingDown {
		return
	}
	for _, r := range m.records {
		switch r.runtime.Status {
		case model.StatusRunning, model.StatusStarting, model.StatusStopping, model.StatusRestarting:
			return
		}
	}
	waiters := m.shutdownWait
	m.shutdownWait = nil
	for _, waiter := range waiters {
		waiter <- opResult{value: struct{}{}}
	}
	m.terminated = true
}

func (m *Manager) rejectPending() {
	err := errors.New("process manager is closed")
	for {
		select {
		case event := <-m.events:
			switch e := event.(type) {
			case restoreCmd:
				e.reply <- opResult{err: err}
			case createCmd:
				e.reply <- opResult{err: err}
			case actionCmd:
				e.reply <- opResult{err: err}
			case deleteCmd:
				e.reply <- opResult{err: err}
			case listCmd:
				e.reply <- opResult{err: err}
			case getCmd:
				e.reply <- opResult{err: err}
			case subscribeCmd:
				close(e.ch)
				e.reply <- opResult{err: err}
			case shutdownCmd:
				e.reply <- opResult{value: struct{}{}}
			}
		default:
			return
		}
	}
}

func (m *Manager) find(id string) *record {
	if r := m.records[id]; r != nil {
		return r
	}
	if resolved := m.names[id]; resolved != "" {
		return m.records[resolved]
	}
	return nil
}

func (m *Manager) info(r *record) model.ProcessInfo {
	return model.ProcessInfo{
		Epoch:         m.epoch,
		Revision:      r.revision,
		Config:        cloneConfig(r.config),
		Desired:       r.desired,
		Runtime:       r.runtime,
		OutGeneration: m.logs.Generation(r.config.Name, model.LogStdout),
		ErrGeneration: m.logs.Generation(r.config.Name, model.LogStderr),
	}
}

func (m *Manager) emit(name string, r *record) {
	m.revision++
	r.revision = m.revision
	m.broadcast(model.Event{Name: name, Data: m.info(r)})
}

func (m *Manager) broadcast(event model.Event) {
	for id, ch := range m.subs {
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- model.Event{Name: "event.lagged"}
			close(ch)
			delete(m.subs, id)
		}
	}
}

func (m *Manager) persist() error {
	state := &model.PersistedState{Version: storage.StateVersion, Processes: make([]model.PersistedProcess, 0, len(m.records))}
	for _, r := range m.records {
		state.Processes = append(state.Processes, model.PersistedProcess{
			ProcessConfig: cloneConfig(r.config),
			Desired:       r.desired,
			Hint:          cloneHint(r.hint),
		})
	}
	sort.Slice(state.Processes, func(i, j int) bool { return state.Processes[i].Name < state.Processes[j].Name })
	return m.store.Save(state)
}

func (m *Manager) persistAsync(action string) error {
	if err := m.persist(); err != nil {
		m.logf("persist after %s: %v", action, err)
		return err
	}
	return nil
}

func (m *Manager) post(event any) {
	select {
	case <-m.done:
		return
	default:
	}
	select {
	case m.events <- event:
	default:
		// Never block the state owner on its own queue. The forwarding
		// goroutine remains cancelable when shutdown closes done.
		go func() {
			select {
			case m.events <- event:
			case <-m.done:
			}
		}()
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}

func cloneConfig(cfg model.ProcessConfig) model.ProcessConfig {
	cfg.Args = append([]string(nil), cfg.Args...)
	cfg.Env = cloneMap(cfg.Env)
	return cfg
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneHint(hint *model.RuntimeHint) *model.RuntimeHint {
	if hint == nil {
		return nil
	}
	copy := *hint
	return &copy
}
