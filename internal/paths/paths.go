package paths

import (
	"errors"
	"os"
	"path/filepath"
)

type Paths struct {
	StateDir  string
	LogsDir   string
	StateFile string
	LockFile  string
	PIDFile   string
	DaemonLog string
	Socket    string
}

func Resolve() (Paths, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		if home == "" {
			return Paths{}, errors.New("cannot determine user home directory")
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	stateDir := filepath.Join(stateHome, "akid")
	socket := filepath.Join(stateDir, "daemon.sock")
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		socket = filepath.Join(runtimeDir, "akid.sock")
	}
	return Paths{
		StateDir:  stateDir,
		LogsDir:   filepath.Join(stateDir, "logs"),
		StateFile: filepath.Join(stateDir, "state.json"),
		LockFile:  filepath.Join(stateDir, "daemon.lock"),
		PIDFile:   filepath.Join(stateDir, "daemon.pid"),
		DaemonLog: filepath.Join(stateDir, "daemon.log"),
		Socket:    socket,
	}, nil
}

func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(p.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(p.LogsDir, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.LogsDir, 0o700)
}
