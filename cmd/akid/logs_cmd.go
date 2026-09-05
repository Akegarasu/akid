package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	akidlog "akid/internal/logging"
	"akid/internal/model"
	"akid/internal/protocol"
	"github.com/spf13/cobra"
)

type logsOptions struct {
	follow bool
	stderr bool
	stdout bool
	lines  int
}

func newLogsCommand(app *application) *cobra.Command {
	options := logsOptions{}
	cmd := &cobra.Command{
		Use:   "logs <name-or-id>",
		Short: "Read or follow process logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.stderr && options.stdout {
				return errors.New("--stdout and --stderr are mutually exclusive")
			}
			if options.lines < 0 {
				return errors.New("--lines cannot be negative")
			}
			return app.runLogs(cmd.Context(), args[0], options)
		},
	}
	flags := cmd.Flags()
	flags.BoolVarP(&options.follow, "follow", "f", false, "follow appended log data")
	flags.BoolVarP(&options.stderr, "stderr", "e", false, "read stderr instead of stdout")
	flags.BoolVar(&options.stdout, "stdout", false, "read stdout (the default)")
	flags.IntVarP(&options.lines, "lines", "n", 100, "number of recent lines to print")
	return cmd
}

func (a *application) runLogs(parent context.Context, id string, options logsOptions) error {
	client, _, err := a.client(parent, true)
	if err != nil {
		return err
	}
	id, err = a.resolveProcessRef(parent, client, id)
	if err != nil {
		return err
	}
	stream := model.LogStdout
	if options.stderr {
		stream = model.LogStderr
	}
	var chunk akidlog.LogChunk
	params := map[string]any{"id": id, "stream": stream, "offset": -(1 << 20), "limit": 1 << 20}
	if err := a.call(parent, client, "log.read", params, &chunk); err != nil {
		return err
	}
	if options.lines > 0 {
		if _, err := a.out.Write(lastLines(chunk.Data, options.lines)); err != nil {
			return err
		}
	}
	if !options.follow {
		return nil
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
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
					fmt.Fprintln(a.errOut, "--- log subscription lagged, resuming ---")
					resume = true
					continue
				}
				if event.Gap {
					fmt.Fprintln(a.errOut, "--- log continuity lost; reading current active file ---")
					cursor, generation = 0, event.Chunk.Generation
					continue
				}
				if _, err := a.out.Write(event.Chunk.Data); err != nil {
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
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	for count > 0 && end > 0 {
		previous := bytes.LastIndexByte(data[:end], '\n')
		count--
		if previous < 0 {
			return data
		}
		if count == 0 {
			return data[previous+1:]
		}
		end = previous
	}
	return data
}
