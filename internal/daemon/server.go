package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

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
}

func NewServer(listener net.Listener, manager *manager.Manager, logs *logging.Service, requestShutdown func()) *Server {
	return &Server{listener: listener, manager: manager, logs: logs, metrics: metrics.NewSampler(), requestShutdown: requestShutdown}
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
		go s.serveConn(conn)
	}
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.listener.Close() })
	return err
}

func (s *Server) serveConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer conn.Close()
	scanner := protocol.NewScanner(conn)
	var writeMu sync.Mutex
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
		go func(req protocol.Request) {
			result, shouldShutdown, err := s.dispatch(ctx, req)
			writeMu.Lock()
			var writeErr error
			if err != nil {
				code, message := errorWire(err)
				writeErr = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Error: &protocol.WireError{Code: code, Message: message}})
			} else {
				writeErr = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: result})
				if errors.Is(writeErr, protocol.ErrMessageTooLarge) {
					writeErr = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Error: &protocol.WireError{Code: "RESPONSE_TOO_LARGE", Message: "response exceeds protocol message limit"}})
				}
			}
			writeMu.Unlock()
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
		if err == nil && value.Purge {
			err = s.logs.Purge(value.Name)
		}
		return value, false, err
	case "log.read":
		var params struct {
			ID     string          `json:"id"`
			Stream model.LogStream `json:"stream"`
			Offset int64           `json:"offset"`
			Limit  int             `json:"limit"`
		}
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, false, err
		}
		info, err := s.manager.Get(ctx, params.ID)
		if err != nil {
			return nil, false, err
		}
		chunk, err := s.logs.Read(logging.LogReadRequest{Name: info.Config.Name, Stream: params.Stream, Offset: params.Offset, Limit: params.Limit})
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
	err = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: map[string]bool{"subscribed": true}})
	writeMu.Unlock()
	if err != nil {
		return
	}
	for event := range events {
		envelope := protocol.EventEnvelope{Protocol: protocol.Version, Event: event.Name}
		if event.Name != "event.lagged" {
			envelope.Data, _ = json.Marshal(event.Data)
		}
		writeMu.Lock()
		err := protocol.WriteMessage(conn, envelope)
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
	events, err := s.logs.Subscribe(subCtx, info.Config.Name, params.Stream, params.Offset, params.Generation)
	if err != nil {
		s.writeManagerError(conn, writeMu, req.ID, err)
		return
	}
	writeMu.Lock()
	err = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: req.ID, Result: map[string]bool{"subscribed": true}})
	writeMu.Unlock()
	if err != nil {
		return
	}
	for event := range events {
		name := "log.append"
		if event.Lagged {
			name = "event.lagged"
		}
		data, _ := json.Marshal(event)
		writeMu.Lock()
		err := protocol.WriteMessage(conn, protocol.EventEnvelope{Protocol: protocol.Version, Event: name, Data: data})
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
	if len(data) == 0 {
		return &manager.Error{Code: manager.CodeInvalidConfig, Message: "params are required"}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return &manager.Error{Code: manager.CodeInvalidConfig, Message: "invalid params: " + err.Error()}
	}
	return nil
}

func (s *Server) writeError(conn net.Conn, mu *sync.Mutex, id json.RawMessage, code, message string) {
	mu.Lock()
	defer mu.Unlock()
	_ = protocol.WriteMessage(conn, protocol.Response{Protocol: protocol.Version, ID: id, Error: &protocol.WireError{Code: code, Message: message}})
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_CANCELED", err.Error()
	}
	return manager.CodeInternal, err.Error()
}
