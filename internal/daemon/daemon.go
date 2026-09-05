package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"akid/internal/executor"
	akidlog "akid/internal/logging"
	"akid/internal/manager"
	"akid/internal/paths"
	"akid/internal/storage"
)

func Run(ctx context.Context, p paths.Paths) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	lock, err := acquireLock(p.LockFile)
	if err != nil {
		return err
	}
	defer lock.Close()

	logFile, err := akidlog.NewRotatingWriter(p.DaemonLog, akidlog.DefaultMaxSize, akidlog.DefaultKeep)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("daemon starting pid=%d", os.Getpid())
	defer logger.Printf("daemon stopped")

	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(p.PIDFile, 0o600); err != nil {
		return err
	}
	defer os.Remove(p.PIDFile)

	logs, err := akidlog.NewService(p.LogsDir, akidlog.DefaultMaxSize, akidlog.DefaultKeep)
	if err != nil {
		return err
	}
	defer logs.Close()
	exec, err := executor.New()
	if err != nil {
		return err
	}
	defer exec.Close()
	// The exclusive instance lock is held before stale socket cleanup, so one
	// daemon cannot unlink another live daemon's socket.
	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", p.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(p.Socket, 0o600); err != nil {
		listener.Close()
		return err
	}
	defer os.Remove(p.Socket)
	// Bind the endpoint before restoring processes. A bind/permission failure
	// must not leave restored children running without a reachable manager.
	mgr, err := manager.New(&storage.FileStore{Path: p.StateFile, Logger: logger}, exec, logs, logger)
	if err != nil {
		return err
	}

	shutdown := make(chan struct{})
	var shutdownOnce sync.Once
	requestShutdown := func() { shutdownOnce.Do(func() { close(shutdown) }) }
	server := NewServer(listener, mgr, logs, requestShutdown)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	logger.Printf("listening on %s", p.Socket)

	var runErr error
	select {
	case <-ctx.Done():
		requestShutdown()
	case <-shutdown:
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("serve: %w", err)
		}
		requestShutdown()
	}
	_ = server.Close()

	stopCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := mgr.Shutdown(stopCtx); err != nil {
		logger.Printf("shutdown processes: %v", err)
		if runErr == nil {
			runErr = err
		}
	}
	return runErr
}
