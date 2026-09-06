package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartCommandUsesCobraForFlagsAndChildArguments(t *testing.T) {
	cmd := newStartCommand(newApplication(&bytes.Buffer{}, &bytes.Buffer{}))
	err := cmd.ParseFlags([]string{
		"python", "worker.py",
		"--name", "worker",
		"--restart=on-failure",
		"--env", "MODE=prod",
		"--stop-timeout", "10s",
		"--", "--name", "child-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Flags().Args()
	if len(args) != 4 || args[0] != "python" || args[1] != "worker.py" || args[2] != "--name" || args[3] != "child-value" {
		t.Fatalf("unexpected positional/child args: %#v", args)
	}
	name, _ := cmd.Flags().GetString("name")
	restart, _ := cmd.Flags().GetString("restart")
	stopTimeout, _ := cmd.Flags().GetDuration("stop-timeout")
	environment, _ := cmd.Flags().GetStringArray("env")
	if name != "worker" || restart != "on-failure" || stopTimeout != 10*time.Second {
		t.Fatalf("unexpected flags: name=%q restart=%q stop=%s", name, restart, stopTimeout)
	}
	if len(environment) != 1 || environment[0] != "MODE=prod" {
		t.Fatalf("unexpected environment flags: %#v", environment)
	}
}

func TestApplyCheckDoesNotConnectToDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "missing-runtime"))
	path := filepath.Join(dir, "akid.toml")
	if err := os.WriteFile(path, []byte("[[process]]\nname='api'\ncommand='true'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newRootCommand(newApplication(&out, &out))
	cmd.SetArgs([]string{"apply", "--check", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "valid configuration: 1 processes\n" {
		t.Fatalf("output: %s", &out)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatalf("validation created state: %v", err)
	}
}

func TestApplyCommandHasUpAlias(t *testing.T) {
	root := newRootCommand(newApplication(&bytes.Buffer{}, &bytes.Buffer{}))
	apply, _, err := root.Find([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	if apply.Name() != "apply" {
		t.Fatalf("up resolved to %q", apply.Name())
	}
}

func TestFishCompletion(t *testing.T) {
	var out bytes.Buffer
	root := newRootCommand(newApplication(&out, &out))
	root.SetArgs([]string{"completion", "fish"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("__akid_perform_completion")) {
		t.Fatalf("fish completion script was not generated: %s", out.String())
	}
}

func TestStartCommandRejectsUnknownFlags(t *testing.T) {
	cmd := newStartCommand(newApplication(&bytes.Buffer{}, &bytes.Buffer{}))
	if err := cmd.ParseFlags([]string{"./server", "--nanme", "api"}); err == nil {
		t.Fatal("expected unknown flag to be rejected")
	}

	cmd = newStartCommand(newApplication(&bytes.Buffer{}, &bytes.Buffer{}))
	if err := cmd.ParseFlags([]string{"./server", "--name", "api", "--", "--port", "8080"}); err != nil {
		t.Fatal(err)
	}
	args := cmd.Flags().Args()
	if len(args) != 3 || args[1] != "--port" || args[2] != "8080" {
		t.Fatalf("child flags after -- were not preserved: %#v", args)
	}
}

func TestParseEnvironment(t *testing.T) {
	environment, err := parseEnvironment([]string{"MODE=prod", "EMPTY="})
	if err != nil {
		t.Fatal(err)
	}
	if environment["MODE"] != "prod" || environment["EMPTY"] != "" {
		t.Fatalf("unexpected environment: %#v", environment)
	}
	if _, err := parseEnvironment([]string{"INVALID"}); err == nil {
		t.Fatal("expected invalid environment error")
	}
}

func TestAbsoluteWorkingDirectory(t *testing.T) {
	resolved, err := absoluteWorkingDirectory("relative-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) || filepath.Base(resolved) != "relative-dir" {
		t.Fatalf("working directory was not normalized: %q", resolved)
	}
}

func TestLastLines(t *testing.T) {
	got := string(lastLines([]byte("one\ntwo\nthree\n"), 2))
	if got != "two\nthree\n" {
		t.Fatalf("lastLines = %q", got)
	}
}
