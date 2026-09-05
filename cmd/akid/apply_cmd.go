package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"akid/internal/config"
	"akid/internal/manager"
	"akid/internal/model"
	"akid/internal/protocol"
	"github.com/spf13/cobra"
)

func newApplyCommand(app *application) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "apply [akid.toml]",
		Short: "Apply process configuration from a TOML file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "akid.toml"
			if len(args) == 1 {
				path = args[0]
			}
			configs, err := config.Load(path)
			if err != nil {
				return err
			}
			if check {
				fmt.Fprintf(app.out, "valid configuration: %d processes\n", len(configs))
				return nil
			}
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			if err := client.RequireCapabilities(cmd.Context(), "config.apply"); err != nil {
				return err
			}
			var result manager.ApplyResult
			if err := app.call(cmd.Context(), client, "config.apply", protocol.ApplyParams{Processes: configs}, &result); err != nil {
				return err
			}
			failed := false
			for _, entry := range result.Processes {
				if entry.Error == nil && entry.Action == "updated" && entry.Process.Runtime.Status == model.StatusStopping {
					info, err := waitAppliedProcess(cmd.Context(), client, entry.Process)
					entry.Process = info
					if err != nil {
						entry.Error = &manager.ApplyError{Code: "APPLY_INCOMPLETE", Message: err.Error()}
					}
				}
				fmt.Fprintf(app.out, "%s %s (%s)\n", entry.Process.Config.Name, entry.Action, entry.Process.Runtime.Status)
				if entry.Error != nil {
					failed = true
					fmt.Fprintf(app.errOut, "%s: %s: %s\n", entry.Process.Config.Name, entry.Error.Code, entry.Error.Message)
				}
			}
			if failed {
				return errors.New("configuration saved, but one or more process actions failed; inspect status and logs")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "validate the configuration without connecting to the daemon")
	return cmd
}

func waitAppliedProcess(parent context.Context, client *protocol.Client, initial model.ProcessInfo) (model.ProcessInfo, error) {
	ctx, cancel := context.WithTimeout(parent, initial.Config.StopTimeout()+10*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	last := initial
	for {
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("update %q did not finish: %w", initial.Config.Name, ctx.Err())
		case <-ticker.C:
			if err := client.Call(ctx, "process.get", map[string]string{"id": initial.Config.ID}, &last); err != nil {
				return last, err
			}
			if last.Runtime.Status == model.StatusRunning && (last.Runtime.PID != initial.Runtime.PID || last.Runtime.StartTime != initial.Runtime.StartTime) {
				return last, nil
			}
			if last.Runtime.Status == model.StatusExited {
				if last.Runtime.ExitCode != nil && *last.Runtime.ExitCode == 0 && last.Desired == model.DesiredStopped {
					return last, nil
				}
				return last, fmt.Errorf("updated process %q exited or failed to start; inspect status and logs", last.Config.Name)
			}
			if last.Runtime.Status == model.StatusStopped {
				return last, fmt.Errorf("updated process %q was stopped before restart completed", last.Config.Name)
			}
		}
	}
}
