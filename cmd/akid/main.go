package main

import (
	"errors"
	"fmt"
	"os"

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
	cmd := newRootCommand(newApplication(os.Stdout, os.Stderr))
	cmd.SetArgs(args)
	return cmd.Execute()
}
