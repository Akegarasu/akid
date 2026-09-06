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
		newCompletionCommand(app),
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

func newCompletionCommand(app *application) *cobra.Command {
	cmd := &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion script",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(app.out)
			case "zsh":
				return root.GenZshCompletion(app.out)
			case "fish":
				return root.GenFishCompletion(app.out, true)
			case "powershell":
				return root.GenPowerShellCompletion(app.out)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
	return cmd
}
