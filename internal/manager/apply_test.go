package manager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"akid/internal/executor"
	"akid/internal/model"
)

func TestApplyCreatesUpdatesAndPreservesUnmentionedProcesses(t *testing.T) {
	mgr, store, exec, original := newTestManager(t, model.RestartNever)
	ctx := context.Background()
	cfg := model.ProcessConfig{Name: "worker", Command: "fake", Env: map[string]string{"MODE": "one"}}
	first, err := mgr.Apply(ctx, []model.ProcessConfig{cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Processes) != 1 || first.Processes[0].Action != "created" || first.Processes[0].Error != nil || exec.startCount() != 1 {
		t.Fatalf("first apply: %+v", first)
	}
	before := first.Processes[0].Process
	// Implicit and explicit default stop timeouts are equivalent.
	cfg.StopTimeoutNS = int64(model.DefaultStopTimeout)
	same, err := mgr.Apply(ctx, []model.ProcessConfig{cfg})
	if err != nil || same.Processes[0].Action != "unchanged" || same.Processes[0].Process.Revision != before.Revision || exec.startCount() != 1 {
		t.Fatalf("non-idempotent apply: %+v %v", same, err)
	}
	cfg.Env["MODE"] = "two"
	updated, err := mgr.Apply(ctx, []model.ProcessConfig{cfg})
	if err != nil || updated.Processes[0].Action != "updated" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	after := waitForStatus(t, mgr, "worker", model.StatusRunning, time.Second)
	if after.Config.ID != before.Config.ID || after.Runtime.PID == before.Runtime.PID || after.Config.Env["MODE"] != "two" || exec.startCount() != 2 {
		t.Fatalf("bad restart: %+v", after)
	}
	untouched, err := mgr.Get(ctx, original.Config.ID)
	if err != nil || untouched.Runtime.Status != model.StatusStopped {
		t.Fatalf("unmentioned process changed: %+v %v", untouched, err)
	}
	saved, err := store.Load()
	if err != nil || len(saved.Processes) != 2 {
		t.Fatalf("saved state: %+v %v", saved, err)
	}
	if _, err := mgr.Stop(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, "worker", model.StatusStopped, time.Second)
	same, err = mgr.Apply(ctx, []model.ProcessConfig{cfg})
	if err != nil || same.Processes[0].Action != "unchanged" || exec.startCount() != 2 {
		t.Fatalf("same config restarted stopped process: %+v %v", same, err)
	}
}

func TestApplyRejectsWholeInvalidBatch(t *testing.T) {
	for _, invalid := range []model.ProcessConfig{
		{Name: "bad"},
		{Name: "worker", Command: "fake"},
		{ID: "provided", Name: "bad", Command: "fake"},
	} {
		mgr, _, exec, _ := newTestManager(t, model.RestartNever)
		if _, err := mgr.Apply(context.Background(), []model.ProcessConfig{{Name: "worker", Command: "fake"}, invalid}); err == nil {
			t.Fatal("accepted invalid batch")
		}
		list, err := mgr.List(context.Background())
		if err != nil || len(list) != 1 || exec.startCount() != 0 {
			t.Fatalf("invalid batch had side effects: %+v %v", list, err)
		}
	}
}

type failingApplyStore struct {
	*memoryStore
	fail bool
	lock sync.Mutex
}

func (s *failingApplyStore) Save(state *model.PersistedState) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.fail {
		return errors.New("injected save failure")
	}
	return s.memoryStore.Save(state)
}

func TestApplyPersistenceFailureLeavesExistingProcessRunning(t *testing.T) {
	store := &failingApplyStore{memoryStore: newMemoryStore()}
	exec := newFakeExecutor()
	mgr, err := New(store, exec, fakeLogs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	cfg := model.ProcessConfig{Name: "api", Command: "fake"}
	first, err := mgr.Apply(context.Background(), []model.ProcessConfig{cfg})
	if err != nil {
		t.Fatal(err)
	}
	before := first.Processes[0].Process
	store.lock.Lock()
	store.fail = true
	store.lock.Unlock()
	cfg.Command = "changed"
	_, err = mgr.Apply(context.Background(), []model.ProcessConfig{cfg, {Name: "new", Command: "fake"}})
	store.lock.Lock()
	store.fail = false
	store.lock.Unlock()
	if err == nil {
		t.Fatal("expected persistence error")
	}
	after, err := mgr.Get(context.Background(), "api")
	if err != nil || after.Config.Command != "fake" || after.Runtime.PID != before.Runtime.PID || !exec.Alive(before.Runtime.PID, before.Runtime.StartTime) || exec.startCount() != 1 {
		t.Fatalf("failed apply changed process: %+v %v", after, err)
	}
	list, _ := mgr.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("failed create retained: %+v", list)
	}
}

type selectiveExecutor struct{ *fakeExecutor }

func (e selectiveExecutor) Start(cfg model.ProcessConfig, paths executor.LogPaths) (*executor.RunningProcess, error) {
	if cfg.Command == "missing" {
		return nil, errors.New("executable not found")
	}
	return e.fakeExecutor.Start(cfg, paths)
}

func TestApplyReportsRuntimeFailuresPerProcess(t *testing.T) {
	exec := selectiveExecutor{newFakeExecutor()}
	mgr, err := New(newMemoryStore(), exec, fakeLogs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	result, err := mgr.Apply(context.Background(), []model.ProcessConfig{{Name: "bad", Command: "missing", Restart: model.RestartNever}, {Name: "good", Command: "fake"}})
	if err != nil || len(result.Processes) != 2 {
		t.Fatalf("apply: %+v %v", result, err)
	}
	if result.Processes[0].Error == nil || result.Processes[0].Error.Code != CodeSpawnFailed || result.Processes[1].Error != nil || result.Processes[1].Process.Runtime.Status != model.StatusRunning {
		t.Fatalf("wrong partial result: %+v", result)
	}
}

func TestConcurrentApplyStartsEachProcessOnce(t *testing.T) {
	mgr, _, exec, _ := newTestManager(t, model.RestartNever)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := mgr.Apply(context.Background(), []model.ProcessConfig{{Name: "worker", Command: "fake"}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if exec.startCount() != 1 {
		t.Fatalf("started %d times", exec.startCount())
	}
}

func TestRestoreCompletesPersistedConfigurationRestart(t *testing.T) {
	store := newMemoryStore()
	exec := newFakeExecutor()
	old, err := exec.Start(model.ProcessConfig{}, executor.LogPaths{})
	if err != nil {
		t.Fatal(err)
	}
	store.state.Processes = []model.PersistedProcess{{ProcessConfig: model.ProcessConfig{ID: "id", Name: "api", Command: "new-config", Restart: model.RestartAlways}, Desired: model.DesiredRunning, Hint: &model.RuntimeHint{PID: old.PID, StartTime: old.StartTime}, RestartPending: true}}
	mgr, err := New(store, exec, fakeLogs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	after := waitForStatus(t, mgr, "api", model.StatusRunning, time.Second)
	if after.Runtime.PID == old.PID || exec.Alive(old.PID, old.StartTime) || exec.startCount() != 2 {
		t.Fatalf("old configuration kept running: %+v", after)
	}
	saved, err := store.Load()
	if err != nil || saved.Processes[0].RestartPending {
		t.Fatalf("pending restart not cleared: %+v %v", saved, err)
	}
}
