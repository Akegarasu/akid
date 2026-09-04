package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"akid/internal/paths"
	"akid/internal/protocol"
)

type application struct {
	out    io.Writer
	errOut io.Writer
}

func newApplication(out, errOut io.Writer) *application {
	return &application{out: out, errOut: errOut}
}

func (a *application) client(ctx context.Context, autoStart bool) (*protocol.Client, paths.Paths, error) {
	resolved, err := paths.Resolve()
	if err != nil {
		return nil, paths.Paths{}, err
	}
	client := protocol.NewClient(resolved.Socket)
	if autoStart {
		if err := ensureDaemon(ctx, resolved, client); err != nil {
			return nil, paths.Paths{}, err
		}
	}
	return client, resolved, nil
}

func (a *application) call(ctx context.Context, client *protocol.Client, method string, params, result any) error {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return client.Call(ctx, method, params, result)
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return client.Call(callCtx, method, params, result)
}

func ensureDaemon(ctx context.Context, p paths.Paths, client *protocol.Client) error {
	pingCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err := client.Call(pingCtx, "daemon.ping", nil, nil)
	cancel()
	if err == nil {
		return nil
	}
	if err := p.Ensure(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.DaemonLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "daemon", "run")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	lastErr := err
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("daemon did not become ready: %w (see %s)", lastErr, p.DaemonLog)
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			lastErr = client.Call(pingCtx, "daemon.ping", nil, nil)
			cancel()
			if lastErr == nil {
				return nil
			}
		}
	}
}
