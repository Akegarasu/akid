package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"akid/internal/executor"
	"akid/internal/model"
	"akid/internal/storage"
)

type memoryStore struct {
	mu        sync.Mutex
	state     model.PersistedState
	failEmpty bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{state: model.PersistedState{Version: storage.StateVersion, Processes: []model.PersistedProcess{}}}
}

func (s *memoryStore) Load() (*model.PersistedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(&s.state), nil
}
func (s *memoryStore) Save(state *model.PersistedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failEmpty && len(state.Processes) == 0 {
		return errors.New("injected empty-state save failure")
	}
	s.state = *cloneState(state)
	return nil
}
func cloneState(state *model.PersistedState) *model.PersistedState {
	data, _ := json.Marshal(state)
	var result model.PersistedState
	_ = json.Unmarshal(data, &result)
	return &result
}

type fakeLogs struct{}

func (fakeLogs) Register(string) error { return nil }
func (fakeLogs) Paths(name string) (string, string) {
	return name + ".out", name + ".err"
}
func (fakeLogs) Generation(string, model.LogStream) uint64 { return 0 }

type fakeProcess struct {
	token uint64
	done  chan executor.ExitResult
	alive bool
}

type fakeExecutor struct {
	mu      sync.Mutex
	nextPID int
	starts  int
	procs   map[int]*fakeProcess
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{nextPID: 100, procs: make(map[int]*fakeProcess)}
}
func (e *fakeExecutor) Start(model.ProcessConfig, executor.LogPaths) (*executor.RunningProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextPID++
	e.starts++
	proc := &fakeProcess{token: uint64(e.nextPID * 10), done: make(chan executor.ExitResult, 1), alive: true}
	e.procs[e.nextPID] = proc
	return &executor.RunningProcess{PID: e.nextPID, StartTime: proc.token, Done: proc.done}, nil
}
func (e *fakeExecutor) Adopt(pid int, token uint64) (*executor.RunningProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	proc := e.procs[pid]
	if proc == nil || !proc.alive || proc.token != token {
		return nil, executor.ErrProcessGone
	}
	return &executor.RunningProcess{PID: pid, StartTime: token, Done: proc.done, Adopted: true}, nil
}
func (e *fakeExecutor) Alive(pid int, token uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	proc := e.procs[pid]
	return proc != nil && proc.alive && proc.token == token
}
func (e *fakeExecutor) SignalGroup(pid int, token uint64, _ bool) error {
	e.mu.Lock()
	proc := e.procs[pid]
	if proc == nil || !proc.alive || proc.token != token {
		e.mu.Unlock()
		return executor.ErrProcessGone
	}
	proc.alive = false
	e.mu.Unlock()
	proc.done <- executor.ExitResult{Code: 143, Known: true}
	close(proc.done)
	return nil
}
func (e *fakeExecutor) Close() error { return nil }
func (e *fakeExecutor) crash(pid, code int) {
	e.mu.Lock()
	proc := e.procs[pid]
	proc.alive = false
	e.mu.Unlock()
	proc.done <- executor.ExitResult{Code: code, Known: true}
	close(proc.done)
}
func (e *fakeExecutor) startCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts
}

func newTestManager(t *testing.T, policy model.RestartPolicy) (*Manager, *memoryStore, *fakeExecutor, model.ProcessInfo) {
	t.Helper()
	store := newMemoryStore()
	exec := newFakeExecutor()
	mgr, err := New(store, exec, fakeLogs{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})
	created, err := mgr.Create(context.Background(), model.ProcessConfig{Name: "api", Command: "fake", Restart: policy})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, store, exec, created
}

func TestStartIsIdempotentAndStopChangesDesiredState(t *testing.T) {
	mgr, store, exec, created := newTestManager(t, model.RestartAlways)
	started, err := mgr.Start(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Runtime.Status != model.StatusRunning || exec.startCount() != 1 {
		t.Fatalf("unexpected start: %#v starts=%d", started, exec.startCount())
	}
	if _, err := mgr.Start(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	if exec.startCount() != 1 {
		t.Fatalf("idempotent start spawned %d processes", exec.startCount())
	}
	if _, err := mgr.Stop(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	stopped := waitForStatus(t, mgr, "api", model.StatusStopped, 2*time.Second)
	if stopped.Desired != model.DesiredStopped {
		t.Fatalf("desired state = %q", stopped.Desired)
	}
	persisted, _ := store.Load()
	if len(persisted.Processes) != 1 || persisted.Processes[0].Desired != model.DesiredStopped || persisted.Processes[0].Hint != nil {
		t.Fatalf("unexpected persisted stop: %#v", persisted)
	}
}

func TestRestartWaitsForExitThenSpawns(t *testing.T) {
	mgr, _, exec, created := newTestManager(t, model.RestartAlways)
	started, err := mgr.Start(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldPID := started.Runtime.PID
	if _, err := mgr.Restart(context.Background(), "api"); err != nil {
		t.Fatal(err)
	}
	running := waitForStatus(t, mgr, "api", model.StatusRunning, 2*time.Second)
	if running.Runtime.PID == oldPID || exec.startCount() != 2 {
		t.Fatalf("restart did not replace process: %#v starts=%d", running, exec.startCount())
	}
}

func TestCrashSchedulesBackoffAndRestarts(t *testing.T) {
	mgr, _, exec, created := newTestManager(t, model.RestartOnFailure)
	started, err := mgr.Start(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec.crash(started.Runtime.PID, 2)
	backoff := waitForStatus(t, mgr, "api", model.StatusExited, time.Second)
	if backoff.Runtime.NextRetryAt.IsZero() {
		t.Fatal("expected next retry time")
	}
	running := waitForStatus(t, mgr, "api", model.StatusRunning, 2*time.Second)
	if running.Runtime.RestartCount != 1 || exec.startCount() != 2 {
		t.Fatalf("unexpected automatic restart: %#v starts=%d", running, exec.startCount())
	}
}

func TestNeverPolicyTurnsCleanExitIntoStoppedDesiredState(t *testing.T) {
	mgr, _, exec, created := newTestManager(t, model.RestartNever)
	started, err := mgr.Start(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec.crash(started.Runtime.PID, 0)
	info := waitForStatus(t, mgr, "api", model.StatusExited, time.Second)
	if info.Desired != model.DesiredStopped || !info.Runtime.NextRetryAt.IsZero() {
		t.Fatalf("unexpected terminal state: %#v", info)
	}
}

func TestDeleteRunningProcessStopsThenRemovesIt(t *testing.T) {
	mgr, _, _, created := newTestManager(t, model.RestartAlways)
	if _, err := mgr.Start(context.Background(), created.Config.ID); err != nil {
		t.Fatal(err)
	}
	result, err := mgr.Delete(context.Background(), "api", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "api" || !result.Purge {
		t.Fatalf("unexpected delete result: %#v", result)
	}
	if _, err := mgr.Get(context.Background(), "api"); err == nil {
		t.Fatal("deleted process still exists")
	} else {
		var managed *Error
		if !errors.As(err, &managed) || managed.Code != CodeNotFound {
			t.Fatalf("unexpected get error: %v", err)
		}
	}
}

func TestDeleteRollbackWhenFinalPersistFails(t *testing.T) {
	mgr, store, _, created := newTestManager(t, model.RestartAlways)
	if _, err := mgr.Start(context.Background(), created.Config.ID); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failEmpty = true
	store.mu.Unlock()
	if _, err := mgr.Delete(context.Background(), "api", false); err == nil {
		t.Fatal("expected delete persistence failure")
	}
	info, err := mgr.Get(context.Background(), "api")
	if err != nil {
		t.Fatalf("process disappeared after rollback: %v", err)
	}
	if info.Runtime.Status != model.StatusStopped {
		t.Fatalf("rolled-back process is not stopped: %#v", info)
	}
	store.mu.Lock()
	store.failEmpty = false
	store.mu.Unlock()
}

func TestLaggedSubscriberIsEvicted(t *testing.T) {
	ch := make(chan model.Event, 1)
	ch <- model.Event{Name: "old"}
	mgr := &Manager{subs: map[uint64]chan model.Event{1: ch}}
	mgr.broadcast(model.Event{Name: "new"})
	if len(mgr.subs) != 0 {
		t.Fatal("lagged subscriber was not evicted")
	}
	event, ok := <-ch
	if !ok || event.Name != "event.lagged" {
		t.Fatalf("expected lagged marker, got %#v open=%v", event, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("lagged subscriber channel was not closed")
	}
}

func TestShutdownStopsProcessesButPreservesRunningIntent(t *testing.T) {
	mgr, store, _, created := newTestManager(t, model.RestartAlways)
	if _, err := mgr.Start(context.Background(), created.Config.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mgr.done:
	case <-time.After(time.Second):
		t.Fatal("manager event loop did not terminate")
	}
	state, _ := store.Load()
	if len(state.Processes) != 1 || state.Processes[0].Desired != model.DesiredRunning || state.Processes[0].Hint != nil {
		t.Fatalf("shutdown lost running intent: %#v", state)
	}
}

func waitForStatus(t *testing.T, mgr *Manager, id string, wanted model.ProcessStatus, timeout time.Duration) model.ProcessInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := mgr.Get(context.Background(), id)
		if err == nil && info.Runtime.Status == wanted {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := mgr.Get(context.Background(), id)
	t.Fatalf("timed out waiting for %s: info=%#v err=%v", wanted, info, err)
	return model.ProcessInfo{}
}
