package storage

import (
	"os"
	"path/filepath"
	"testing"

	"akid/internal/model"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := &FileStore{Path: path}
	initial, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if initial.Version != StateVersion || len(initial.Processes) != 0 {
		t.Fatalf("unexpected initial state: %#v", initial)
	}

	state := &model.PersistedState{Processes: []model.PersistedProcess{{
		ProcessConfig: model.ProcessConfig{
			ID:      "id-1",
			Name:    "api",
			Command: "/bin/api",
			Args:    []string{"--port", "8080"},
			Env:     map[string]string{"MODE": "test"},
			Restart: model.RestartAlways,
		},
		Desired: model.DesiredRunning,
		Hint:    &model.RuntimeHint{PID: 42, StartTime: 1234},
	}}}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != StateVersion || len(loaded.Processes) != 1 {
		t.Fatalf("unexpected loaded state: %#v", loaded)
	}
	got := loaded.Processes[0]
	if got.Name != "api" || got.Desired != model.DesiredRunning || got.Hint == nil || got.Hint.StartTime != 1234 {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestFileStoreRecoversFromBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := &FileStore{Path: path}
	first := &model.PersistedState{Processes: []model.PersistedProcess{{
		ProcessConfig: model.ProcessConfig{ID: "1", Name: "first", Command: "one", Restart: model.RestartNever},
		Desired:       model.DesiredStopped,
	}}}
	second := &model.PersistedState{Processes: []model.PersistedProcess{{
		ProcessConfig: model.ProcessConfig{ID: "2", Name: "second", Command: "two", Restart: model.RestartNever},
		Desired:       model.DesiredStopped,
	}}}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Processes) != 1 || recovered.Processes[0].Name != "first" {
		t.Fatalf("did not recover previous generation: %#v", recovered)
	}
	// Load repairs the main file, so recovery does not repeat forever.
	repaired, err := loadStateFile(path)
	if err != nil || repaired.Processes[0].Name != "first" {
		t.Fatalf("main state was not repaired: state=%#v err=%v", repaired, err)
	}
}

func TestFileStoreRejectsDuplicateNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := &FileStore{Path: path}
	state := &model.PersistedState{Processes: []model.PersistedProcess{
		{ProcessConfig: model.ProcessConfig{ID: "1", Name: "api", Command: "one", Restart: model.RestartNever}, Desired: model.DesiredStopped},
		{ProcessConfig: model.ProcessConfig{ID: "2", Name: "api", Command: "two", Restart: model.RestartNever}, Desired: model.DesiredStopped},
	}}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}
