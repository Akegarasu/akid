package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"akid/internal/model"
)

const StateVersion = 1

type Store interface {
	Load() (*model.PersistedState, error)
	Save(*model.PersistedState) error
}

type PrintfLogger interface {
	Printf(format string, args ...any)
}

type FileStore struct {
	Path   string
	Logger PrintfLogger
}

type versionError struct{ version int }

func (e *versionError) Error() string { return fmt.Sprintf("unsupported state version %d", e.version) }

func (s *FileStore) Load() (*model.PersistedState, error) {
	state, err := loadStateFile(s.Path)
	if err == nil {
		return state, nil
	}
	var unsupported *versionError
	if errors.As(err, &unsupported) {
		// Never replace a newer state format with an older backup.
		return nil, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		s.logf("state load failed, trying backup: %v", err)
	}

	backupPath := s.Path + ".bak"
	backup, backupErr := loadStateFile(backupPath)
	if backupErr == nil {
		if repairErr := writeStateAtomic(s.Path, backup); repairErr != nil {
			return nil, fmt.Errorf("load backup after main state error (%v), but repair main state: %w", err, repairErr)
		}
		s.logf("recovered state from %s", backupPath)
		return backup, nil
	}
	if errors.Is(err, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return &model.PersistedState{Version: StateVersion, Processes: []model.PersistedProcess{}}, nil
	}
	return nil, fmt.Errorf("load state: %w (backup unavailable: %v)", err, backupErr)
}

func (s *FileStore) Save(state *model.PersistedState) error {
	if state == nil {
		return errors.New("state is nil")
	}
	state.Version = StateVersion
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Keep the last known-valid generation. Re-parse before backing up so an
	// externally corrupted main file never replaces a good backup.
	if previous, err := loadStateFile(s.Path); err == nil {
		if err := writeStateAtomic(s.Path+".bak", previous); err != nil {
			return fmt.Errorf("save state backup: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// Seed a backup on the first save as well; otherwise corruption before
		// the second state transition would still have no recovery path.
		if _, backupErr := os.Stat(s.Path + ".bak"); errors.Is(backupErr, os.ErrNotExist) {
			if err := writeStateAtomic(s.Path+".bak", state); err != nil {
				return fmt.Errorf("seed state backup: %w", err)
			}
		} else if backupErr != nil {
			return fmt.Errorf("inspect state backup: %w", backupErr)
		}
	} else {
		s.logf("not backing up invalid current state: %v", err)
	}
	return writeStateAtomic(s.Path, state)
}

func loadStateFile(path string) (*model.PersistedState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var state model.PersistedState
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	if err := dec.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple JSON values", filepath.Base(path))
		}
		return nil, fmt.Errorf("decode %s trailing data: %w", filepath.Base(path), err)
	}
	if state.Version != StateVersion {
		return nil, &versionError{version: state.Version}
	}
	if state.Processes == nil {
		state.Processes = []model.PersistedProcess{}
	}
	seenIDs := make(map[string]struct{}, len(state.Processes))
	seenNames := make(map[string]struct{}, len(state.Processes))
	for i := range state.Processes {
		p := &state.Processes[i]
		if err := p.ProcessConfig.NormalizeAndValidate(); err != nil {
			return nil, fmt.Errorf("invalid process %d: %w", i, err)
		}
		if p.ID == "" {
			return nil, fmt.Errorf("invalid process %q: id is required", p.Name)
		}
		if p.Desired != model.DesiredRunning && p.Desired != model.DesiredStopped {
			return nil, fmt.Errorf("invalid process %q: invalid desired state %q", p.Name, p.Desired)
		}
		if _, ok := seenIDs[p.ID]; ok {
			return nil, fmt.Errorf("duplicate process id %q", p.ID)
		}
		if _, ok := seenNames[p.Name]; ok {
			return nil, fmt.Errorf("duplicate process name %q", p.Name)
		}
		seenIDs[p.ID] = struct{}{}
		seenNames[p.Name] = struct{}{}
	}
	return &state, nil
}

func writeStateAtomic(path string, state *model.PersistedState) error {
	if state == nil {
		return errors.New("state is nil")
	}
	state.Version = StateVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *FileStore) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}
