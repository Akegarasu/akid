package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"akid/internal/paths"
	"akid/internal/startup"
	"github.com/spf13/cobra"
)

func newStartupCommand(app *application) *cobra.Command {
	cmd := &cobra.Command{Use: "startup", Short: "Manage the systemd user service for future logins/boots", Args: cobra.NoArgs}
	for _, action := range []string{"install", "uninstall"} {
		sub := &cobra.Command{
			Use: action, Short: action + " the akid systemd user service", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if runtime.GOOS != "linux" {
					return fmt.Errorf("startup requires Linux and systemd")
				}
				configHome, err := os.UserConfigDir()
				if err != nil {
					return err
				}
				executable, err := os.Executable()
				if err != nil {
					return err
				}
				executable, err = filepath.EvalSymlinks(executable)
				if err != nil {
					return err
				}
				p, err := paths.Resolve()
				if err != nil {
					return err
				}
				svc := startup.Service{ConfigHome: configHome, Executable: executable, StateHome: filepath.Dir(p.StateDir), RuntimeDir: os.Getenv("XDG_RUNTIME_DIR"), Run: startup.RunCommand}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				if action == "uninstall" {
					if err := svc.Uninstall(ctx); err != nil {
						return err
					}
					fmt.Fprintln(app.out, "startup disabled; running daemon and managed processes are unchanged")
					return nil
				}
				if err := svc.Install(ctx); err != nil {
					return err
				}
				fmt.Fprintf(app.out, "startup enabled: %s\n", svc.Path())
				fmt.Fprintln(app.out, "The service starts on the next user session/boot. For an immediate handover, run akid daemon stop, then systemctl --user start akid.service after the daemon exits.")
				account, err := user.Current()
				if err != nil {
					fmt.Fprintf(app.errOut, "unable to check linger: %v\n", err)
					return nil
				}
				enabled, err := svc.Linger(ctx, account.Uid)
				if err != nil {
					fmt.Fprintf(app.errOut, "unable to check linger: %v\n", err)
				} else if !enabled {
					fmt.Fprintf(app.errOut, "linger is disabled; to start without a login and keep running after logout, run: loginctl enable-linger %s\n", account.Username)
				}
				return nil
			},
		}
		cmd.AddCommand(sub)
	}
	return cmd
}
