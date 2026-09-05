package tui

import (
	"strings"
	"testing"
	"time"

	"akid/internal/model"
)

func TestProcessDetailIncludesIdentityCommandAndRuntimeFields(t *testing.T) {
	exit := 17
	started := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := model.ProcessInfo{
		Config:        model.ProcessConfig{ID: "id-api", Name: "api", Command: "/bin/server", Args: []string{"--port", "8080"}, Cwd: "/srv/api", Restart: model.RestartNever, Env: map[string]string{"MODE": "prod"}},
		Desired:       model.DesiredStopped,
		Runtime:       model.RuntimeState{Status: model.StatusExited, PID: 42, StartedAt: started, ExitCode: &exit},
		OutGeneration: 11,
		ErrGeneration: 12,
	}
	joined := strings.Join(processDetailRows(info, model.ProcessMetrics{}, 160, started), "\n")
	for _, want := range []string{"id-api", "/bin/server", "--port 8080", "/srv/api", "MODE=prod", "17", "Log stdout", "11", "Log stderr", "12"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("detail missing %q:\n%s", want, joined)
		}
	}
}
