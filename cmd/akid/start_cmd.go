package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"akid/internal/manager"
	"akid/internal/model"
	"akid/internal/protocol"
	"github.com/spf13/cobra"
)

type startOptions struct {
	name        string
	cwd         string
	restart     string
	environment []string
	stopTimeout time.Duration
}

func newStartCommand(app *application) *cobra.Command {
	options := startOptions{}
	cmd := &cobra.Command{
		Use:   "start <command-or-existing-name> [positional-args...] [-- child-flags...]",
		Short: "Create and start a process, or start an existing process",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runStart(cmd.Context(), args, options)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&options.name, "name", "", "name for a newly created process")
	flags.StringVar(&options.cwd, "cwd", "", "child working directory (default: current directory)")
	flags.StringVar(&options.restart, "restart", string(model.RestartAlways), "restart policy: always, on-failure, or never")
	flags.StringArrayVar(&options.environment, "env", nil, "environment override in KEY=VALUE form (repeatable)")
	flags.DurationVar(&options.stopTimeout, "stop-timeout", model.DefaultStopTimeout, "graceful stop timeout")
	return cmd
}

func (a *application) runStart(ctx context.Context, args []string, options startOptions) error {
	client, _, err := a.client(ctx, true)
	if err != nil {
		return err
	}
	if options.name == "" {
		if len(args) != 1 {
			return errors.New("--name is required when creating a process")
		}
		resolvedID, err := a.resolveProcessRef(ctx, client, args[0])
		if err != nil {
			return err
		}
		var info model.ProcessInfo
		if err := a.call(ctx, client, "process.start", map[string]string{"id": resolvedID}, &info); err != nil {
			return err
		}
		printOneLine(a.out, info)
		return nil
	}

	var existing model.ProcessInfo
	err = a.call(ctx, client, "process.get", map[string]string{"id": options.name}, &existing)
	if err == nil {
		if err := a.call(ctx, client, "process.start", map[string]string{"id": options.name}, &existing); err != nil {
			return err
		}
		printOneLine(a.out, existing)
		return nil
	}
	var remote *protocol.RemoteError
	if !errors.As(err, &remote) || remote.Code != manager.CodeNotFound {
		return err
	}

	commandArgs, err := expandStartCommand(args)
	if err != nil {
		return err
	}
	cwd, err := absoluteWorkingDirectory(options.cwd)
	if err != nil {
		return err
	}
	environment, err := parseEnvironment(options.environment)
	if err != nil {
		return err
	}
	cfg := model.ProcessConfig{
		Name:          options.name,
		Command:       resolveExecutable(commandArgs[0]),
		Args:          append([]string(nil), commandArgs[1:]...),
		Cwd:           cwd,
		Env:           environment,
		Restart:       model.RestartPolicy(options.restart),
		StopTimeoutNS: int64(options.stopTimeout),
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return err
	}
	var created model.ProcessInfo
	if err := a.call(ctx, client, "process.create", map[string]any{"config": cfg}, &created); err != nil {
		// Another concurrent client may have created the same name.
		if !errors.As(err, &remote) || remote.Code != manager.CodeNameConflict {
			return err
		}
	}
	var started model.ProcessInfo
	if err := a.call(ctx, client, "process.start", map[string]string{"id": options.name}, &started); err != nil {
		return err
	}
	printOneLine(a.out, started)
	return nil
}

func (a *application) resolveProcessRef(ctx context.Context, client *protocol.Client, ref string) (string, error) {
	var info model.ProcessInfo
	if err := a.call(ctx, client, "process.get", map[string]string{"id": ref}, &info); err == nil {
		return info.Config.ID, nil
	} else {
		var remote *protocol.RemoteError
		if !errors.As(err, &remote) || remote.Code != manager.CodeNotFound {
			return "", err
		}
	}
	index, err := strconv.Atoi(ref)
	if err != nil || index < 1 {
		return ref, nil
	}
	var list []model.ProcessInfo
	if err := a.call(ctx, client, "process.list", nil, &list); err != nil {
		return "", err
	}
	if index > len(list) {
		return "", &protocol.RemoteError{Code: manager.CodeNotFound, Message: fmt.Sprintf("process number %d not found", index)}
	}
	return list[index-1].Config.ID, nil
}

func parseEnvironment(values []string) (map[string]string, error) {
	environment := make(map[string]string, len(values))
	for _, item := range values {
		name, value, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --env %q; expected KEY=VALUE", item)
		}
		environment[name] = value
	}
	return environment, nil
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
