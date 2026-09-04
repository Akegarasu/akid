package main

import (
	"akid/internal/tui"
	"github.com/spf13/cobra"
)

func newUICommand(app *application) *cobra.Command {
	var mouse bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the interactive terminal interface",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := app.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			return tui.Run(cmd.Context(), client, app.out, tui.Options{Mouse: mouse})
		},
	}
	cmd.Flags().BoolVar(&mouse, "mouse", false, "capture mouse clicks and wheel events")
	return cmd
}
