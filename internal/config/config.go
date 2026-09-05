// Package config reads declarative process definitions. Only the daemon applies
// them; parsing a file never changes running processes or persistent state.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"akid/internal/model"
	"github.com/pelletier/go-toml/v2"
)

const MaxFileSize = 8 << 20

type document struct {
	Processes []process `toml:"process"`
}

type process struct {
	Name        string            `toml:"name"`
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Cwd         string            `toml:"cwd"`
	Env         map[string]string `toml:"env"`
	Restart     string            `toml:"restart"`
	StopTimeout string            `toml:"stop_timeout"`
}

func Load(path string) ([]model.ProcessConfig, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("config exceeds %d bytes", MaxFileSize)
	}
	return Parse(data, filepath.Dir(abs))
}

// Parse resolves cwd relative to the configuration file's directory. An omitted
// cwd uses that directory, so the invoking shell's directory has no effect.
func Parse(data []byte, baseDir string) ([]model.ProcessConfig, error) {
	var doc document
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}
	configs := make([]model.ProcessConfig, 0, len(doc.Processes))
	names := make(map[string]bool)
	for i, entry := range doc.Processes {
		cfg := model.ProcessConfig{Name: entry.Name, Command: entry.Command, Args: entry.Args, Env: entry.Env, Restart: model.RestartPolicy(entry.Restart), Cwd: entry.Cwd}
		if !filepath.IsAbs(cfg.Cwd) {
			cfg.Cwd = filepath.Join(base, cfg.Cwd)
		}
		cfg.Cwd = filepath.Clean(cfg.Cwd)
		if entry.StopTimeout != "" {
			duration, err := time.ParseDuration(entry.StopTimeout)
			if err != nil || duration <= 0 {
				return nil, fmt.Errorf("process %d (%q): stop_timeout must be a positive duration", i+1, cfg.Name)
			}
			cfg.StopTimeoutNS = int64(duration)
		}
		if err := cfg.NormalizeAndValidate(); err != nil {
			return nil, fmt.Errorf("process %d (%q): %w", i+1, cfg.Name, err)
		}
		if names[cfg.Name] {
			return nil, fmt.Errorf("duplicate process name %q", cfg.Name)
		}
		names[cfg.Name] = true
		configs = append(configs, cfg)
	}
	return configs, nil
}
