package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"akid/internal/logging"
	"akid/internal/manager"
	"akid/internal/metrics"
	"akid/internal/model"
	"akid/internal/protocol"
)

type Server struct {
	listener        net.Listener
	manager         *manager.Manager
	logs            *logging.Service
	metrics         *metrics.Sampler
	requestShutdown func()
	closeOnce       sync.Once
	mu              sync.Mutex
	connections     map[net.Conn]struct{}
	closed          bool
	writeTimeout    time.Duration
	ctx             context.Context
	cancel          context.CancelFunc
}

func NewServer(listener net.Listener, manager *manager.Manager, logs *logging.Service, requestShutdown func()) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		listener: listener, manager: manager, logs: logs,
		metrics: metrics.NewSampler(), requestShutdown: requestShutdown,
		connections: make(map[net.Conn]struct{}), writeTimeout: 5 * time.Second,
		ctx: ctx, cancel: cancel,
	}
}

func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.connections[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.connections, conn)
				s.mu.Unlock()
			}()
			s.serveConn(conn)
		}()
	}
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		s.cancel()
		err = s.listener.Close()
		for conn := range s.connections {
			_ = conn.Close()
		}
	})
	return err
}

func (s *Server) serveConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	defer conn.Close()
	scanner := protocol.NewScanner(conn)
	var writeMu sync.Mutex
	// Bound outstanding requests on a pipelined control connection. Process
	// lifecycle work is serialized by the manager regardless of this limit.
	requests := make(chan struct{}, 32)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(conn, &writeMu, nil, "INVALID_REQUEST", "invalid JSON request")
			continue
		}
		if req.Protocol != protocol.Version {
			s.writeError(conn, &writeMu, req.ID, "PROTOCOL_MISMATCH", fmt.Sprintf("unsupported protocol %d", req.Protocol))
			continue
		}
		if req.Method == "event.subscribe" {
			s.serveEventSubscription(ctx, conn, &writeMu, req)
			return
		}
		if req.Method == "log.subscribe" {
			s.serveLogSubscription(ctx, conn, &writeMu, req)
			return
		}
		select {
		case requests <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func(req protocol.Request) {
			defer func() { <-requests }()
			result, shouldShutdown, err := s.dispatch(ctx, req)
			writeMu.Lock()
			var writeErr error
			if err != nil {
				code, message := errorWire(err)
				writeErr = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Error: &protocol.WireError{Code: code, Message: message}})
			} else {
				writeErr = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: result})
				if errors.Is(writeErr, protocol.ErrMessageTooLarge) {
					writeErr = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Error: &protocol.WireError{Code: "RESPONSE_TOO_LARGE", Message: "response exceeds protocol message limit"}})
				}
			}
			writeMu.Unlock()
			if writeErr != nil {
				cancel()
				_ = conn.Close()
			}
			if shouldShutdown && err == nil && writeErr == nil {
				s.requestShutdown()
			}
		}(req)
	}
}

func (s *Server) dispatch(ctx context.Context, req protocol.Request) (any, bool, error) {
	switch req.Method {
	case "daemon.ping":
		return map[string]any{"pid": os.Getpid(), "version": protocol.Version}, false, nil
	case "daemon.capabilities":
		return protocol.SupportedCapabilities(), false, nil
	case "config.apply":
		var params protocol.ApplyParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		if params.Processes == nil {
			return nil, false, &manager.Error{Code: manager.CodeInvalidConfig, Message: "processes array is required"}
		}
		value, err := s.manager.Apply(ctx, params.Processes)
		return value, false, err
	case "daemon.shutdown":
		return map[string]bool{"accepted": true}, true, nil
	case "process.list":
		value, err := s.manager.List(ctx)
		return value, false, err
	case "process.metrics":
		values, err := s.manager.List(ctx)
		if err != nil {
			return nil, false, err
		}
		return s.metrics.Sample(values), false, nil
	case "process.get":
		var params idParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		value, err := s.manager.Get(ctx, params.ID)
		return value, false, err
	case "process.create":
		var params struct {
			Config model.ProcessConfig `json:"config"`
		}
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		value, err := s.manager.Create(ctx, params.Config)
		return value, false, err
	case "process.start", "process.stop", "process.restart":
		var params idParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		var value model.ProcessInfo
		var err error
		switch req.Method {
		case "process.start":
			value, err = s.manager.Start(ctx, params.ID)
		case "process.stop":
			value, err = s.manager.Stop(ctx, params.ID)
		case "process.restart":
			value, err = s.manager.Restart(ctx, params.ID)
		}
		return value, false, err
	case "process.delete":
		var params struct {
			ID    string `json:"id"`
			Purge bool   `json:"purge"`
		}
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		value, err := s.manager.Delete(ctx, params.ID, params.Purge)
		return value, false, err
	case "log.read":
		var params struct {
			ID     string          `json:"id"`
			Stream model.LogStream `json:"stream"`
			Offset int64           `json:"offset"`
			Limit  int             `json:"limit"`
			Align  bool            `json:"align"`
		}
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		info, err := s.manager.Get(ctx, params.ID)
		if err != nil {
			return nil, false, err
		}
		chunk, err := s.logs.Read(logging.LogReadRequest{Name: info.Config.Name, Owner: info.Config.ID, Stream: params.Stream, Offset: params.Offset, Limit: params.Limit, Align: params.Align})
		return chunk, false, err
	default:
		return nil, false, &manager.Error{Code: "METHOD_NOT_FOUND", Message: fmt.Sprintf("method %q not found", req.Method)}
	}
}

func (s *Server) serveEventSubscription(ctx context.Context, conn net.Conn, writeMu *sync.Mutex, req protocol.Request) {
	subCtx, cancel := subscriptionContext(ctx, conn)
	defer cancel()
	events, err := s.manager.Subscribe(subCtx)
	if err != nil {
		s.writeManagerError(conn, writeMu, req.ID, err)
		return
	}
	writeMu.Lock()
	err = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: map[string]bool{"subscribed": true}})
	writeMu.Unlock()
	if err != nil {
		return
	}
	for event := range events {
		envelope := protocol.EventEnvelope{Protocol: protocol.Version, Event: event.Name}
		if event.Snapshot != nil {
			envelope.Data, _ = json.Marshal(event.Snapshot)
		} else if event.Name != "event.lagged" {
			envelope.Data, _ = json.Marshal(event.Data)
		}
		writeMu.Lock()
		err := s.writeMessage(conn, envelope)
		writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *Server) serveLogSubscription(ctx context.Context, conn net.Conn, writeMu *sync.Mutex, req protocol.Request) {
	subCtx, cancel := subscriptionContext(ctx, conn)
	defer cancel()
	var params protocol.LogSubscribeParams
	if err := decodeParams(req.Params, &params); err != nil {
		s.writeManagerError(conn, writeMu, req.ID, err)
		return
	}
	info, err := s.manager.Get(ctx, params.ID)
	if err != nil {
		s.writeManagerError(conn, writeMu, req.ID, err)
		return
	}
	events, err := s.logs.Subscribe(subCtx, info.Config.Name, params.Stream, params.Offset, params.Generation, info.Config.ID)
	if err != nil {
		s.writeManagerError(conn, writeMu, req.ID, err)
		return
	}
	writeMu.Lock()
	err = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: map[string]bool{"subscribed": true}})
	writeMu.Unlock()
	if err != nil {
		return
	}
	for event := range events {
		name := "log.append"
		if event.Gap {
			name = "log.gap"
		}
		if event.Lagged {
			name = "event.lagged"
		}
		data, _ := json.Marshal(event)
		writeMu.Lock()
		err := s.writeMessage(conn, protocol.EventEnvelope{Protocol: protocol.Version, Event: name, Data: data})
		writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

func subscriptionContext(parent context.Context, conn net.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	// Subscription connections are server-write-only after the initial
	// request. A blocking read is therefore a cheap disconnect detector and
	// ensures idle subscriptions are removed when a client goes away.
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancel()
	}()
	return ctx, cancel
}

type idParams struct {
	ID string `json:"id"`
}

func decodeParams(data json.RawMessage, target any) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return &manager.Error{Code: manager.CodeInvalidConfig, Message: "params are required"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &manager.Error{Code: manager.CodeInvalidConfig, Message: "invalid params: " + err.Error()}
	}
	return nil
}

func (s *Server) writeMessage(conn net.Conn, value any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		return err
	}
	return protocol.WriteMessage(conn, value)
}

func (s *Server) writeError(conn net.Conn, mu *sync.Mutex, id json.RawMessage, code, message string) {
	mu.Lock()
	defer mu.Unlock()
	_ = s.writeMessage(conn, protocol.Response{Protocol: protocol.Version, ID: id, Error: &protocol.WireError{Code: code, Message: message}})
}

func (s *Server) writeManagerError(conn net.Conn, mu *sync.Mutex, id json.RawMessage, err error) {
	code, message := errorWire(err)
	s.writeError(conn, mu, id, code, message)
}

func errorWire(err error) (string, string) {
	var managed *manager.Error
	if errors.As(err, &managed) {
		return managed.Code, managed.Message
	}
	if errors.Is(err, os.ErrNotExist) {
		return "LOG_NOT_FOUND", err.Error()
	}
	if errors.Is(err, logging.ErrInvalidRequest) {
		return manager.CodeInvalidConfig, err.Error()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_CANCELED", err.Error()
	}
	return manager.CodeInternal, err.Error()
}
