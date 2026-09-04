//go:build linux

package executor

import (
	"os"
	"os/signal"
	"syscall"
)

func notifySIGCHLD(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGCHLD) }
func stopSIGCHLD(ch chan<- os.Signal)   { signal.Stop(ch) }
