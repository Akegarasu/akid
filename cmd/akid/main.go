package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"akid/internal/daemon"
	akidlog "akid/internal/logging"
	"akid/internal/manager"
	"akid/internal/model"
	"akid/internal/paths"
	"akid/internal/protocol"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		var remote *protocol.RemoteError
		if errors.As(err, &remote) {
			fmt.Fprintf(os.Stderr, "akid: %s: %s\n", remote.Code, remote.Message)
		} else {
			fmt.Fprintf(os.Stderr, "akid: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return errors.New("command is required")
	}
	p, err := paths.Resolve()
	if err != nil {
		return err
	}
	client := protocol.NewClient(p.Socket)

	switch args[0] {
	case "daemon":
		return runDaemonCommand(args[1:], p, client)
	case "start":
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runStart(args[1:], client)
	case "list", "ls":
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runList(client)
	case "status":
		if len(args) != 2 {
			return errors.New("usage: akid status <name-or-id>")
		}
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runStatus(client, args[1])
	case "stop", "restart":
		if len(args) != 2 {
			return fmt.Errorf("usage: akid %s <name-or-id>", args[0])
		}
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runProcessAction(client, args[0], args[1])
	case "delete":
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runDelete(args[1:], client)
	case "logs":
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		return runLogs(args[1:], client)
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runDaemonCommand(args []string, p paths.Paths, client *protocol.Client) error {
	if len(args) == 0 {
		return errors.New("usage: akid daemon <start|stop|status|run>")
	}
	switch args[0] {
	case "run":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return daemon.Run(ctx, p)
	case "start":
		if err := ensureDaemon(p, client); err != nil {
			return err
		}
		fmt.Println("akid daemon is running")
		return nil
	case "status":
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var result struct {
			PID int `json:"pid"`
		}
		if err := client.Call(ctx, "daemon.ping", nil, &result); err != nil {
			return errors.New("akid daemon is not running")
		}
		fmt.Printf("akid daemon is running (pid %d)\n", result.PID)
		return nil
	case "stop":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.Call(ctx, "daemon.shutdown", nil, nil); err != nil {
			return errors.New("akid daemon is not running")
		}
		fmt.Println("akid daemon shutdown requested")
		return nil
	default:
		return fmt.Errorf("unknown daemon command %q", args[0])
	}
}

func ensureDaemon(p paths.Paths, client *protocol.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := client.Call(ctx, "daemon.ping", nil, nil)
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

	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		lastErr = client.Call(ctx, "daemon.ping", nil, nil)
		cancel()
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("daemon did not become ready: %w (see %s)", lastErr, p.DaemonLog)
}

type startOptions struct {
	name        string
	command     string
	args        []string
	cwd         string
	restart     model.RestartPolicy
	env         map[string]string
	stopTimeout time.Duration
}

func runStart(args []string, client *protocol.Client) error {
	opts, err := parseStart(args)
	if err != nil {
		return err
	}
	if opts.name == "" {
		if len(opts.args) != 0 {
			return errors.New("--name is required when creating a process")
		}
		var info model.ProcessInfo
		if err := call(client, "process.start", map[string]string{"id": opts.command}, &info); err != nil {
			return err
		}
		printOneLine(info)
		return nil
	}

	var existing model.ProcessInfo
	err = call(client, "process.get", map[string]string{"id": opts.name}, &existing)
	if err == nil {
		if err := call(client, "process.start", map[string]string{"id": opts.name}, &existing); err != nil {
			return err
		}
		printOneLine(existing)
		return nil
	}
	var remote *protocol.RemoteError
	if !errors.As(err, &remote) || remote.Code != manager.CodeNotFound {
		return err
	}
	opts.cwd, err = absoluteWorkingDirectory(opts.cwd)
	if err != nil {
		return err
	}
	cfg := model.ProcessConfig{
		Name:          opts.name,
		Command:       opts.command,
		Args:          opts.args,
		Cwd:           opts.cwd,
		Env:           opts.env,
		Restart:       opts.restart,
		StopTimeoutNS: int64(opts.stopTimeout),
	}
	var created model.ProcessInfo
	if err := call(client, "process.create", map[string]any{"config": cfg}, &created); err != nil {
		// Another concurrent client may have created the same name.
		if !errors.As(err, &remote) || remote.Code != manager.CodeNameConflict {
			return err
		}
	}
	var started model.ProcessInfo
	if err := call(client, "process.start", map[string]string{"id": opts.name}, &started); err != nil {
		return err
	}
	printOneLine(started)
	return nil
}

func absoluteWorkingDirectory(value string) (string, error) {
	var (
		resolved string
		err      error
	)
	if value == "" {
		resolved, err = os.Getwd()
	} else {
		resolved, err = filepath.Abs(value)
	}
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func parseStart(args []string) (startOptions, error) {
	opts := startOptions{restart: model.RestartAlways, env: make(map[string]string)}
	childMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			childMode = true
			continue
		}
		if !childMode {
			key, inline, hasInline := strings.Cut(arg, "=")
			switch key {
			case "--name", "--cwd", "--restart", "--env", "--stop-timeout":
				value := inline
				if !hasInline {
					i++
					if i >= len(args) {
						return opts, fmt.Errorf("%s requires a value", key)
					}
					value = args[i]
				}
				switch key {
				case "--name":
					opts.name = value
				case "--cwd":
					opts.cwd = value
				case "--restart":
					opts.restart = model.RestartPolicy(value)
				case "--env":
					name, envValue, ok := strings.Cut(value, "=")
					if !ok || name == "" {
						return opts, fmt.Errorf("invalid --env %q; expected KEY=VALUE", value)
					}
					opts.env[name] = envValue
				case "--stop-timeout":
					duration, err := time.ParseDuration(value)
					if err != nil || duration <= 0 {
						return opts, fmt.Errorf("invalid stop timeout %q", value)
					}
					opts.stopTimeout = duration
				}
				continue
			}
		}
		if !childMode && strings.HasPrefix(arg, "-") {
			return opts, fmt.Errorf("unknown start option %q (use -- before child flags)", arg)
		}
		if opts.command == "" {
			opts.command = arg
		} else {
			opts.args = append(opts.args, arg)
		}
	}
	if opts.command == "" {
		return opts, errors.New("usage: akid start <command> [args...] --name <name>")
	}
	cfg := model.ProcessConfig{Name: opts.name, Command: opts.command, Restart: opts.restart, StopTimeoutNS: int64(opts.stopTimeout)}
	if opts.name != "" {
		if err := cfg.NormalizeAndValidate(); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func runProcessAction(client *protocol.Client, action, id string) error {
	var initial model.ProcessInfo
	if err := call(client, "process."+action, map[string]string{"id": id}, &initial); err != nil {
		return err
	}
	isComplete := func(info model.ProcessInfo) bool {
		if action == "stop" {
			return info.Runtime.Status == model.StatusStopped
		}
		return info.Runtime.Status == model.StatusRunning &&
			(info.Runtime.PID != initial.Runtime.PID || info.Runtime.StartTime != initial.Runtime.StartTime || initial.Runtime.Status == model.StatusRunning)
	}
	if isComplete(initial) {
		printOneLine(initial)
		return nil
	}

	timeout := initial.Config.StopTimeout() + 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	last := initial
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %q did not complete within %s (current status: %s)", action, id, timeout, last.Runtime.Status)
		case <-ticker.C:
			if err := client.Call(ctx, "process.get", map[string]string{"id": id}, &last); err != nil {
				return err
			}
			if isComplete(last) {
				printOneLine(last)
				return nil
			}
		}
	}
}

func runList(client *protocol.Client) error {
	var list []model.ProcessInfo
	if err := call(client, "process.list", nil, &list); err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tSTATUS\tPID\tRESTARTS\tCOMMAND")
	for _, info := range list {
		pid := "-"
		if info.Runtime.PID > 0 {
			pid = strconv.Itoa(info.Runtime.PID)
		}
		status := string(info.Runtime.Status)
		if !info.Runtime.NextRetryAt.IsZero() {
			status = "backoff"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n", info.Config.Name, status, pid, info.Runtime.RestartCount, info.Config.Command)
	}
	return writer.Flush()
}

func runStatus(client *protocol.Client, id string) error {
	var info model.ProcessInfo
	if err := call(client, "process.get", map[string]string{"id": id}, &info); err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runDelete(args []string, client *protocol.Client) error {
	if len(args) == 0 {
		return errors.New("usage: akid delete <name-or-id> [--purge]")
	}
	id := ""
	purge := false
	for _, arg := range args {
		switch arg {
		case "--purge":
			purge = true
		default:
			if id != "" {
				return errors.New("usage: akid delete <name-or-id> [--purge]")
			}
			id = arg
		}
	}
	var result map[string]any
	if err := call(client, "process.delete", map[string]any{"id": id, "purge": purge}, &result); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", id)
	return nil
}

func runLogs(args []string, client *protocol.Client) error {
	if len(args) == 0 {
		return errors.New("usage: akid logs <name-or-id> [-f] [--stderr] [-n lines]")
	}
	id := ""
	follow := false
	stream := model.LogStdout
	lines := 100
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--follow":
			follow = true
		case "--stderr", "-e":
			stream = model.LogStderr
		case "--stdout":
			stream = model.LogStdout
		case "-n", "--lines":
			i++
			if i >= len(args) {
				return errors.New("-n requires a value")
			}
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 0 {
				return fmt.Errorf("invalid line count %q", args[i])
			}
			lines = value
		default:
			if id != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			id = args[i]
		}
	}
	if id == "" {
		return errors.New("process name or id is required")
	}
	var chunk akidlog.LogChunk
	params := map[string]any{"id": id, "stream": stream, "offset": -(1 << 20), "limit": 1 << 20}
	if err := call(client, "log.read", params, &chunk); err != nil {
		return err
	}
	if lines > 0 {
		_, _ = os.Stdout.Write(lastLines(chunk.Data, lines))
	}
	if !follow {
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cursor, generation := chunk.EndOffset, chunk.Generation
	for {
		events, err := client.SubscribeLogs(ctx, protocol.LogSubscribeParams{ID: id, Stream: stream, Offset: cursor, Generation: generation})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		resume := false
		for !resume {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-events:
				if !ok {
					return errors.New("log subscription closed")
				}
				if event.Lagged {
					fmt.Fprintln(os.Stderr, "--- log subscription lagged, resuming ---")
					resume = true
					continue
				}
				if _, err := os.Stdout.Write(event.Chunk.Data); err != nil {
					return err
				}
				cursor = event.Chunk.EndOffset
				generation = event.Chunk.Generation
			}
		}
	}
}

func lastLines(data []byte, count int) []byte {
	if count <= 0 || len(data) == 0 {
		return nil
	}
	trimmed := bytes.TrimSuffix(data, []byte("\n"))
	parts := bytes.Split(trimmed, []byte("\n"))
	if len(parts) > count {
		parts = parts[len(parts)-count:]
	}
	result := bytes.Join(parts, []byte("\n"))
	if len(result) > 0 {
		result = append(result, '\n')
	}
	return result
}

func call(client *protocol.Client, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return client.Call(ctx, method, params, result)
}

func printOneLine(info model.ProcessInfo) {
	pid := "-"
	if info.Runtime.PID > 0 {
		pid = strconv.Itoa(info.Runtime.PID)
	}
	fmt.Printf("%s %s pid=%s\n", info.Config.Name, info.Runtime.Status, pid)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `akid - lightweight local process supervisor

Usage:
  akid start <command> [args...] --name <name> [options]
  akid start <existing-name>
  akid list
  akid status <name-or-id>
  akid stop <name-or-id>
  akid restart <name-or-id>
  akid delete <name-or-id> [--purge]
  akid logs <name-or-id> [-f] [--stderr] [-n lines]
  akid daemon <start|stop|status>

Start options:
  --cwd <dir>              working directory (default: current directory)
  --restart <policy>       always, on-failure, or never (default: always)
  --env KEY=VALUE          environment override; may be repeated
  --stop-timeout <duration> (default: 5s)
  --                       remaining arguments belong to the child process`)
}
