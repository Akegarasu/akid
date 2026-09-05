package startup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testService(t *testing.T) (Service, *[]string) {
	t.Helper()
	var calls []string
	dir := t.TempDir()
	s := Service{ConfigHome: filepath.Join(dir, "config"), Executable: filepath.Join(dir, "akid"), StateHome: filepath.Join(dir, "state"), Run: func(_ context.Context, cmd string, args ...string) (string, error) {
		calls = append(calls, cmd+" "+strings.Join(args, " "))
		return "no\n", nil
	}}
	return s, &calls
}

func TestInstallAndUninstallPreserveRuntime(t *testing.T) {
	s, calls := testService(t)
	ctx := context.Background()
	for range 2 {
		if err := s.Install(ctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Restart=on-failure", "KillMode=mixed", "UMask=0077", "WantedBy=default.target", " daemon run"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("unit lacks %q: %s", want, data)
		}
	}
	if enabled, err := s.Linger(ctx, "1000"); err != nil || enabled {
		t.Fatalf("linger: %v %v", enabled, err)
	}
	for range 2 {
		if err := s.Uninstall(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit retained: %v", err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable akid.service", "systemctl --user daemon-reload", "systemctl --user enable akid.service", "loginctl show-user 1000 --property=Linger --value", "systemctl --user disable akid.service", "systemctl --user daemon-reload"}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("unexpected commands: %v", *calls)
	}
}

func TestUnmanagedUnitIsPreserved(t *testing.T) {
	s, calls := testService(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("[Service]\nExecStart=/custom/akid\n")
	if err := os.WriteFile(s.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(context.Background()); err == nil {
		t.Fatal("overwrote custom unit")
	}
	if err := s.Uninstall(context.Background()); err == nil {
		t.Fatal("removed custom unit")
	}
	after, _ := os.ReadFile(s.Path())
	if string(after) != string(data) || len(*calls) != 0 {
		t.Fatal("modified custom unit or systemd state")
	}
}

func TestUnitEscapesSystemdExpansion(t *testing.T) {
	s, _ := testService(t)
	s.Executable = filepath.Join(t.TempDir(), `with space%$"`, "akid")
	unit, err := s.Unit()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), `with space%%$$\"`) {
		t.Fatalf("unescaped executable: %s", unit)
	}
	s.Executable += "\nExecStart=/bad"
	if _, err := s.Unit(); err == nil {
		t.Fatal("accepted newline in path")
	}
}

func TestSystemctlFailureIsReportedAndRetryable(t *testing.T) {
	s, _ := testService(t)
	run := s.Run
	s.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("no user bus") }
	if err := s.Install(context.Background()); err == nil {
		t.Fatal("ignored reload failure")
	}
	s.Run = run
	if err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
}
