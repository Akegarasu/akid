//go:build linux

package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"akid/internal/model"
)

func testLinuxExecutor(t *testing.T) *LinuxExecutor {
	t.Helper()
	value, err := New()
	if err != nil {
		t.Fatal(err)
	}
	e := value.(*LinuxExecutor)
	t.Cleanup(func() { e.Close() })
	return e
}

func startShell(t *testing.T, e *LinuxExecutor, script, dir string) *RunningProcess {
	t.Helper()
	proc, err := e.Start(model.ProcessConfig{Command: "/bin/sh", Args: []string{"-c", script}, Cwd: dir, StopTimeoutNS: int64(200 * time.Millisecond)}, LogPaths{Stdout: filepath.Join(dir, "out"), Stderr: filepath.Join(dir, "err")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.SignalGroup(proc.PID, proc.StartTime, true)
		select {
		case <-proc.Done:
		case <-time.After(3 * time.Second):
			t.Error("process cleanup timed out")
		}
	})
	return proc
}

func awaitExit(t *testing.T, proc *RunningProcess) ExitResult {
	t.Helper()
	select {
	case result, open := <-proc.Done:
		if !open {
			t.Fatal("exit result missing")
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("process group did not exit")
	}
	return ExitResult{}
}

func awaitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file not ready: %s", path)
	return ""
}

func TestQuickExitPreservesStatus(t *testing.T) {
	e := testLinuxExecutor(t)
	for i := 0; i < 20; i++ {
		proc := startShell(t, e, fmt.Sprintf("exit %d", i%3), t.TempDir())
		result := awaitExit(t, proc)
		if !result.Known || result.Code != i%3 || proc.StartTime == 0 {
			t.Fatalf("wrong quick exit: %+v identity=%d", result, proc.StartTime)
		}
	}
}

func TestLeaderExitCleansTermIgnoringDescendant(t *testing.T) {
	e := testLinuxExecutor(t)
	dir := t.TempDir()
	proc := startShell(t, e, `sh -c 'trap "" TERM; echo ready > ready; while :; do sleep 1; done' & echo $! > child; while [ ! -s ready ]; do sleep 0.01; done; exit 7`, dir)
	child, err := strconv.Atoi(awaitFile(t, filepath.Join(dir, "child")))
	if err != nil {
		t.Fatal(err)
	}
	result := awaitExit(t, proc)
	if !result.Known || result.Code != 7 {
		t.Fatalf("lost leader result: %+v", result)
	}
	if stat, err := readProcessStat(child); err == nil && stat.live() {
		t.Fatalf("descendant %d survived group completion", child)
	}
}

func TestStopStillSignalsGroupAfterLeaderExit(t *testing.T) {
	e := testLinuxExecutor(t)
	dir := t.TempDir()
	proc := startShell(t, e, `sh -c 'trap "" TERM; echo ready > ready; while :; do sleep 1; done' & echo $! > child; wait`, dir)
	awaitFile(t, filepath.Join(dir, "ready"))
	if err := e.SignalGroup(proc.PID, proc.StartTime, false); err != nil {
		t.Fatal(err)
	}
	if err := e.SignalGroup(proc.PID, proc.StartTime+1, true); !errors.Is(err, ErrProcessGone) {
		t.Fatal("accepted wrong process identity")
	}
	result := awaitExit(t, proc)
	if !result.Known || result.Code != 143 {
		t.Fatalf("wrong stop result: %+v", result)
	}
}

func TestTrackedMembersRejectsReusedGroup(t *testing.T) {
	watch := &groupWatch{startTime: 10, members: map[int]uint64{123: 10, 124: 11}}
	table := map[int]processStat{123: {pid: 123, group: 123, startTime: 50, state: "S"}, 124: {pid: 124, group: 123, startTime: 51, state: "S"}}
	if got := trackedMembers(123, watch, table); len(got) != 0 {
		t.Fatalf("accepted reused group: %+v", got)
	}
	table[124] = processStat{pid: 124, group: 123, startTime: 11, state: "S"}
	delete(table, 123)
	if got := trackedMembers(123, watch, table); len(got) != 1 {
		t.Fatal("lost group anchored by known member")
	}
}

func TestAdoptedLeaderExitCleansKnownMembers(t *testing.T) {
	// The fixture runs in another process so its child is not ours to waitpid.
	if os.Getenv("AKID_ADOPT_FIXTURE") == "1" {
		cmd := exec.Command("/bin/sh", "-c", `sh -c 'trap "" TERM; echo ready > ready; while :; do sleep 1; done' & echo $! > member; wait`)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile("leader", []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		_ = cmd.Wait()
		os.Exit(0)
	}
	dir := t.TempDir()
	fixture := exec.Command(os.Args[0], "-test.run=^TestAdoptedLeaderExitCleansKnownMembers$")
	fixture.Dir = dir
	fixture.Env = append(os.Environ(), "AKID_ADOPT_FIXTURE=1")
	if err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Process.Kill(); _ = fixture.Wait() })
	pid, err := strconv.Atoi(awaitFile(t, filepath.Join(dir, "leader")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	awaitFile(t, filepath.Join(dir, "ready"))
	stat, err := readProcessStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	e := testLinuxExecutor(t)
	proc, err := e.Adopt(pid, stat.startTime)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.watchers[pid].stopTimeout = 200 * time.Millisecond
	e.mu.Unlock()
	if err := e.SignalGroup(pid, stat.startTime, false); err != nil {
		t.Fatal(err)
	}
	result := awaitExit(t, proc)
	if result.Known {
		t.Fatal("invented adopted exit code")
	}
	member, err := strconv.Atoi(awaitFile(t, filepath.Join(dir, "member")))
	if err != nil {
		t.Fatal(err)
	}
	if stat, err := readProcessStat(member); err == nil && stat.live() {
		t.Fatal("adopted group member survived")
	}
}
