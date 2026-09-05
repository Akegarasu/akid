package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newRootCommand(app *application) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "akid",
		Short:         "Lightweight local process supervisor",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.SetOut(app.out)
	cmd.SetErr(app.errOut)
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(
		newStartCommand(app),
		newApplyCommand(app),
		newStartupCommand(app),
		newListCommand(app),
		newStatusCommand(app),
		newActionCommand(app, "stop"),
		newActionCommand(app, "restart"),
		newDeleteCommand(app),
		newLogsCommand(app),
		newUICommand(app),
		newDaemonCommand(app),
		&cobra.Command{
			Use:   "version",
			Short: "Print the akid version",
			Args:  cobra.NoArgs,
			Run: func(*cobra.Command, []string) {
				fmt.Fprintln(app.out, version)
			},
		},
	)
	return cmd
}
