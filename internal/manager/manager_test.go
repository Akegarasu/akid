package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (fakeLogs) Register(string, ...string) error  { return nil }
func (fakeLogs) Remove(string, string, bool) error { return nil }
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

func TestRestoreFinishesInterruptedStop(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			store := newMemoryStore()
			exec := newFakeExecutor()
			proc, err := exec.Start(model.ProcessConfig{}, executor.LogPaths{})
			if err != nil {
				t.Fatal(err)
			}
			token := proc.StartTime
			if stale {
				token++
			}
			store.state.Processes = []model.PersistedProcess{{ProcessConfig: model.ProcessConfig{ID: "id", Name: "api", Command: "fake", Restart: model.RestartAlways}, Desired: model.DesiredStopped, Hint: &model.RuntimeHint{PID: proc.PID, StartTime: token}}}
			mgr, err := New(store, exec, fakeLogs{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			waitForStatus(t, mgr, "api", model.StatusStopped, time.Second)
			if alive := exec.Alive(proc.PID, proc.StartTime); alive != stale {
				t.Fatalf("identity safety/stop failed: alive=%v stale=%v", alive, stale)
			}
			state, err := store.Load()
			if err != nil || state.Processes[0].Hint != nil || state.Processes[0].Desired != model.DesiredStopped {
				t.Fatalf("bad recovered state: %+v %v", state, err)
			}
			if exec.startCount() != 1 {
				t.Fatal("recovery spawned a stopped process")
			}
		})
	}
}

func TestSubscriptionSnapshotAndDeletionAreOrdered(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			mgr, _, _, created := newTestManager(t, model.RestartNever)
			if running {
				if _, err := mgr.Start(context.Background(), created.Config.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			events, err := mgr.Subscribe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			first := <-events
			if first.Snapshot == nil || len(first.Snapshot.Processes) != 1 {
				t.Fatalf("missing initial snapshot: %+v", first)
			}
			if _, err := mgr.Delete(ctx, created.Config.ID, false); err != nil {
				t.Fatal(err)
			}
			lastRevision := first.Snapshot.Revision
			for {
				select {
				case event := <-events:
					if event.Data.Epoch != first.Snapshot.Epoch || event.Data.Revision <= lastRevision {
						t.Fatalf("unordered event: %+v", event)
					}
					lastRevision = event.Data.Revision
					if event.Name == "process.deleted" {
						return
					}
				case <-ctx.Done():
					t.Fatal("deletion event missing")
				}
			}
		})
	}
}

type blockingLogs struct {
	fakeLogs
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	owner   string
}

func (l *blockingLogs) Register(_ string, owners ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != "" {
		return errors.New("name still owned")
	}
	l.owner = owners[0]
	return nil
}

func (l *blockingLogs) Remove(_, owner string, purge bool) error {
	if purge {
		close(l.entered)
		<-l.release
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if owner != l.owner {
		return errors.New("wrong owner")
	}
	l.owner = ""
	return nil
}

func TestDeleteKeepsNameReservedUntilLogCleanup(t *testing.T) {
	logs := &blockingLogs{entered: make(chan struct{}), release: make(chan struct{})}
	mgr, err := New(newMemoryStore(), newFakeExecutor(), logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := model.ProcessConfig{Name: "api", Command: "fake"}
	if _, err := mgr.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { _, err := mgr.Delete(ctx, "api", true); deleted <- err }()
	select {
	case <-logs.entered:
	case <-ctx.Done():
		t.Fatal("cleanup did not begin")
	}
	created := make(chan error, 1)
	go func() { _, err := mgr.Create(ctx, cfg); created <- err }()
	select {
	case err := <-created:
		close(logs.release)
		t.Fatalf("create finished during purge: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(logs.release)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}
