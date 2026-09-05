package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"akid/internal/model"
)

func TestLoadResolvesPathsAndPreservesArguments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "akid.toml")
	data := `[[process]]
name = "api"
command = "./server"
args = ["--port", "8080", "two words", "$literal"]
cwd = "services/api"
restart = "on-failure"
stop_timeout = "750ms"
[process.env]
MODE = "development"
EMPTY = ""
[[process]]
name = "worker"
command = "python"
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	configs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs: %+v", configs)
	}
	api, worker := configs[0], configs[1]
	if api.Cwd != filepath.Join(dir, "services/api") || api.Command != "./server" || api.Args[2] != "two words" || api.Args[3] != "$literal" || api.StopTimeout() != 750*time.Millisecond || api.Restart != model.RestartOnFailure || api.Env["MODE"] != "development" {
		t.Fatalf("wrong config: %+v", api)
	}
	if worker.Cwd != dir || worker.Restart != model.RestartAlways || worker.StopTimeout() != model.DefaultStopTimeout {
		t.Fatalf("wrong defaults: %+v", worker)
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	for name, source := range map[string]string{
		"unknown top level": "processes = []",
		"unknown field":     "[[process]]\nname='api'\ncommand='true'\nrestatr='always'",
		"duplicate name":    "[[process]]\nname='api'\ncommand='true'\n[[process]]\nname='api'\ncommand='false'",
		"missing command":   "[[process]]\nname='api'",
		"wrong env type":    "[[process]]\nname='api'\ncommand='true'\n[process.env]\nPORT=8080",
		"invalid env name":  "[[process]]\nname='api'\ncommand='true'\n[process.env]\n'A=B'='x'",
		"zero timeout":      "[[process]]\nname='api'\ncommand='true'\nstop_timeout='0s'",
		"negative timeout":  "[[process]]\nname='api'\ncommand='true'\nstop_timeout='-1s'",
		"overflow timeout":  "[[process]]\nname='api'\ncommand='true'\nstop_timeout='999999999999999999h'",
		"syntax":            "[[process]",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source), t.TempDir()); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestLoadRejectsOversizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akid.toml")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", MaxFileSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted oversized file")
	}
}
