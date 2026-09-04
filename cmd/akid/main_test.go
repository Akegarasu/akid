package main

import (
	"path/filepath"
	"testing"
	"time"

	"akid/internal/model"
)

func TestParseStartOptionsAroundChildArguments(t *testing.T) {
	opts, err := parseStart([]string{"python", "worker.py", "--name", "worker", "--restart=on-failure", "--env", "MODE=prod", "--stop-timeout", "10s", "--", "--name", "child-value"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.command != "python" || opts.name != "worker" || opts.restart != model.RestartOnFailure || opts.stopTimeout != 10*time.Second {
		t.Fatalf("unexpected options: %#v", opts)
	}
	if len(opts.args) != 3 || opts.args[0] != "worker.py" || opts.args[1] != "--name" || opts.args[2] != "child-value" {
		t.Fatalf("unexpected child args: %#v", opts.args)
	}
	if opts.env["MODE"] != "prod" {
		t.Fatalf("unexpected env: %#v", opts.env)
	}
}

func TestParseStartRejectsUnknownOptionBeforeCommand(t *testing.T) {
	if _, err := parseStart([]string{"--nanme", "api", "./server"}); err == nil {
		t.Fatal("expected typo in manager option to be rejected")
	}
	if _, err := parseStart([]string{"./server", "--port", "8080", "--name", "api"}); err == nil {
		t.Fatal("expected child flag without -- separator to be rejected")
	}
	opts, err := parseStart([]string{"./server", "--name", "api", "--", "--port", "8080"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.args) != 2 || opts.args[0] != "--port" {
		t.Fatalf("child flags after -- were not preserved: %#v", opts.args)
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
