package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"akid/internal/logging"
	"akid/internal/model"
)

const (
	Version        = 2
	MaxMessageSize = 16 << 20
)

var ErrMessageTooLarge = errors.New("protocol message exceeds 16 MB")

type Request struct {
	Protocol int             `json:"protocol"`
	ID       json.RawMessage `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Protocol int             `json:"protocol"`
	ID       json.RawMessage `json:"id"`
	Result   any             `json:"result,omitempty"`
	Error    *WireError      `json:"error,omitempty"`
}

type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return e.Message }

type EventEnvelope struct {
	Protocol int             `json:"protocol"`
	Event    string          `json:"event"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type Client struct {
	Socket string
	nextID atomic.Uint64
}

func NewClient(socket string) *Client { return &Client{Socket: socket} }

func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	id := c.nextID.Add(1)
	idJSON, _ := json.Marshal(id)
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	request := Request{Protocol: Version, ID: idJSON, Method: method, Params: paramsJSON}
	if err := WriteMessage(conn, request); err != nil {
		return err
	}

	scanner := NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return errors.New("daemon closed the connection")
	}
	var wire struct {
		Protocol int             `json:"protocol"`
		ID       json.RawMessage `json:"id"`
		Result   json.RawMessage `json:"result"`
		Error    *WireError      `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &wire); err != nil {
		return err
	}
	if wire.Protocol != Version {
		return fmt.Errorf("unsupported daemon protocol %d", wire.Protocol)
	}
	if !bytes.Equal(bytes.TrimSpace(wire.ID), idJSON) {
		return errors.New("daemon response id mismatch")
	}
	if wire.Error != nil {
		return &RemoteError{Code: wire.Error.Code, Message: wire.Error.Message}
	}
	if result == nil || len(wire.Result) == 0 || bytes.Equal(wire.Result, []byte("null")) {
		return nil
	}
	return json.Unmarshal(wire.Result, result)
}

func (c *Client) SubscribeEvents(ctx context.Context) (<-chan model.Event, error) {
	out := make(chan model.Event, 1024)
	conn, scanner, stopContextWatch, err := c.openSubscription(ctx, "event.subscribe", nil)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(out)
		defer conn.Close()
		defer stopContextWatch()
		for scanner.Scan() {
			var envelope EventEnvelope
			if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
				return
			}
			if envelope.Event == "event.lagged" {
				select {
				case out <- model.Event{Name: "event.lagged"}:
				case <-ctx.Done():
				}
				return
			}
			var info model.ProcessInfo
			if envelope.Event == "process.snapshot" {
				var snapshot model.ProcessSnapshot
				if json.Unmarshal(envelope.Data, &snapshot) != nil {
					return
				}
				select {
				case out <- model.Event{Name: envelope.Event, Snapshot: &snapshot}:
				case <-ctx.Done():
					return
				}
				continue
			}
			if json.Unmarshal(envelope.Data, &info) != nil {
				return
			}
			select {
			case out <- model.Event{Name: envelope.Event, Data: info}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

type LogSubscribeParams struct {
	ID         string          `json:"id"`
	Stream     model.LogStream `json:"stream"`
	Offset     int64           `json:"offset"`
	Generation uint64          `json:"generation"`
}

func (c *Client) SubscribeLogs(ctx context.Context, params LogSubscribeParams) (<-chan logging.LogEvent, error) {
	out := make(chan logging.LogEvent, 1024)
	conn, scanner, stopContextWatch, err := c.openSubscription(ctx, "log.subscribe", params)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(out)
		defer conn.Close()
		defer stopContextWatch()
		for scanner.Scan() {
			var envelope EventEnvelope
			if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
				return
			}
			if envelope.Event == "event.lagged" {
				select {
				case out <- logging.LogEvent{Lagged: true}:
				case <-ctx.Done():
				}
				return
			}
			if envelope.Event != "log.append" && envelope.Event != "log.gap" {
				continue
			}
			var event logging.LogEvent
			if json.Unmarshal(envelope.Data, &event) != nil {
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *Client) openSubscription(ctx context.Context, method string, params any) (net.Conn, *bufio.Scanner, func(), error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup := func() {
		stopCancel()
		_ = conn.Close()
	}
	id := c.nextID.Add(1)
	idJSON, _ := json.Marshal(id)
	paramsJSON, err := marshalParams(params)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if err := WriteMessage(conn, Request{Protocol: Version, ID: idJSON, Method: method, Params: paramsJSON}); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	scanner := NewScanner(conn)
	if !scanner.Scan() {
		cleanup()
		if scanner.Err() != nil {
			return nil, nil, nil, scanner.Err()
		}
		return nil, nil, nil, errors.New("daemon closed subscription")
	}
	var response struct {
		Protocol int             `json:"protocol"`
		ID       json.RawMessage `json:"id"`
		Error    *WireError      `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if response.Protocol != Version {
		cleanup()
		return nil, nil, nil, fmt.Errorf("unsupported daemon protocol %d", response.Protocol)
	}
	if !bytes.Equal(bytes.TrimSpace(response.ID), idJSON) {
		cleanup()
		return nil, nil, nil, errors.New("daemon response id mismatch")
	}
	if response.Error != nil {
		cleanup()
		return nil, nil, nil, &RemoteError{Code: response.Error.Code, Message: response.Error.Message}
	}
	return conn, scanner, func() { stopCancel() }, nil
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	data, err := json.Marshal(params)
	return data, err
}

func WriteMessage(conn net.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > MaxMessageSize {
		return ErrMessageTooLarge
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func NewScanner(conn net.Conn) *bufio.Scanner {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), MaxMessageSize+1)
	return scanner
}
