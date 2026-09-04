package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"akid/internal/daemon"
	"akid/internal/paths"
	"github.com/spf13/cobra"
)

func newDaemonCommand(app *application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the local akid daemon",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "start",
			Short: "Start the daemon",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if _, _, err := app.client(cmd.Context(), true); err != nil {
					return err
				}
				fmt.Fprintln(app.out, "akid daemon is running")
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show daemon status",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				client, _, err := app.client(cmd.Context(), false)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Second)
				defer cancel()
				var result struct {
					PID int `json:"pid"`
				}
				if err := client.Call(ctx, "daemon.ping", nil, &result); err != nil {
					return errors.New("akid daemon is not running")
				}
				fmt.Fprintf(app.out, "akid daemon is running (pid %d)\n", result.PID)
				return nil
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop the daemon and managed processes",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				client, _, err := app.client(cmd.Context(), false)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
				defer cancel()
				if err := client.Call(ctx, "daemon.shutdown", nil, nil); err != nil {
					return errors.New("akid daemon is not running")
				}
				fmt.Fprintln(app.out, "akid daemon shutdown requested")
				return nil
			},
		},
		newDaemonRunCommand(),
	)
	return cmd
}

func newDaemonRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := paths.Resolve()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return daemon.Run(ctx, resolved)
		},
	}
}
