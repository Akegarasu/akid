package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"akid/internal/model"
	"akid/internal/protocol"
	"github.com/spf13/cobra"
)

func newListCommand(app *application) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "ps"},
		Short:   "List managed processes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			var list []model.ProcessInfo
			if err := app.call(cmd.Context(), client, "process.list", nil, &list); err != nil {
				return err
			}
			writer := tabwriter.NewWriter(app.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(writer, "#\tNAME\tSTATUS\tPID\tRESTARTS\tCOMMAND")
			for index, info := range list {
				pid := "-"
				if info.Runtime.PID > 0 {
					pid = strconv.Itoa(info.Runtime.PID)
				}
				status := string(info.Runtime.Status)
				if !info.Runtime.NextRetryAt.IsZero() {
					status = "backoff"
				}
				fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%d\t%s\n", index+1, info.Config.Name, status, pid, info.Runtime.RestartCount, info.Config.Command)
			}
			return writer.Flush()
		},
	}
}

func newStatusCommand(app *application) *cobra.Command {
	return &cobra.Command{
		Use:   "status <name-or-id>",
		Short: "Show process details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			resolved, err := app.resolveProcessRef(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			var info model.ProcessInfo
			if err := app.call(cmd.Context(), client, "process.get", map[string]string{"id": resolved}, &info); err != nil {
				return err
			}
			data, err := json.MarshalIndent(info, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(app.out, string(data))
			return nil
		},
	}
}

func newActionCommand(app *application, action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <name-or-id>",
		Short: actionDescription(action),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			resolved, err := app.resolveProcessRef(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			return app.runProcessAction(cmd.Context(), client, action, resolved)
		},
	}
}

func actionDescription(action string) string {
	if action == "stop" {
		return "Stop a managed process"
	}
	return "Restart a managed process"
}

func (a *application) runProcessAction(ctx context.Context, client *protocol.Client, action, id string) error {
	var initial model.ProcessInfo
	if err := a.call(ctx, client, "process."+action, map[string]string{"id": id}, &initial); err != nil {
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
		printOneLine(a.out, initial)
		return nil
	}

	timeout := initial.Config.StopTimeout() + 10*time.Second
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	last := initial
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%s %q did not complete within %s (current status: %s)", action, id, timeout, last.Runtime.Status)
		case <-ticker.C:
			if err := client.Call(waitCtx, "process.get", map[string]string{"id": id}, &last); err != nil {
				return err
			}
			if isComplete(last) {
				printOneLine(a.out, last)
				return nil
			}
		}
	}
}

func newDeleteCommand(app *application) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Stop and remove a managed process",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			resolved, err := app.resolveProcessRef(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			var result map[string]any
			if err := app.call(cmd.Context(), client, "process.delete", map[string]any{"id": resolved, "purge": purge}, &result); err != nil {
				return err
			}
			fmt.Fprintf(app.out, "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove process log files")
	return cmd
}

func printOneLine(out io.Writer, info model.ProcessInfo) {
	pid := "-"
	if info.Runtime.PID > 0 {
		pid = strconv.Itoa(info.Runtime.PID)
	}
	fmt.Fprintf(out, "%s %s pid=%s\n", info.Config.Name, info.Runtime.Status, pid)
}
