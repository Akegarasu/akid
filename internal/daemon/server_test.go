package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
	"time"

	"akid/internal/executor"
	akidlog "akid/internal/logging"
	"akid/internal/manager"
	"akid/internal/model"
	"akid/internal/protocol"
	"akid/internal/storage"
)

type noProcessExecutor struct{}

func (noProcessExecutor) Start(model.ProcessConfig, executor.LogPaths) (*executor.RunningProcess, error) {
	panic("unexpected process start")
}
func (noProcessExecutor) Adopt(int, uint64) (*executor.RunningProcess, error) {
	return nil, executor.ErrProcessGone
}
func (noProcessExecutor) Alive(int, uint64) bool              { return false }
func (noProcessExecutor) SignalGroup(int, uint64, bool) error { return executor.ErrProcessGone }
func (noProcessExecutor) Close() error                        { return nil }

func TestServerDispatchOverSocket(t *testing.T) {
	dir := t.TempDir()
	logs, err := akidlog.NewService(filepath.Join(dir, "logs"), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	mgr, err := manager.New(&storage.FileStore{Path: filepath.Join(dir, "state.json")}, noProcessExecutor{}, logs, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(listener, mgr, logs, func() {})
	defer server.Close()
	go func() { _ = server.Serve() }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scanner := protocol.NewScanner(conn)

	cfg := model.ProcessConfig{Name: "api", Command: "/bin/api", Restart: model.RestartNever}
	create := roundTrip(t, conn, scanner, 1, "process.create", map[string]any{"config": cfg})
	if create.Error != nil {
		t.Fatalf("create failed: %#v", create.Error)
	}
	list := roundTrip(t, conn, scanner, 2, "process.list", nil)
	if list.Error != nil {
		t.Fatalf("list failed: %#v", list.Error)
	}
	data, err := json.Marshal(list.Result)
	if err != nil {
		t.Fatal(err)
	}
	var processes []model.ProcessInfo
	if err := json.Unmarshal(data, &processes); err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 || processes[0].Config.Name != "api" {
		t.Fatalf("unexpected list: %#v", processes)
	}
	metricsResponse := roundTrip(t, conn, scanner, 3, "process.metrics", nil)
	if metricsResponse.Error != nil {
		t.Fatalf("metrics failed: %#v", metricsResponse.Error)
	}
	data, err = json.Marshal(metricsResponse.Result)
	if err != nil {
		t.Fatal(err)
	}
	var sampled []model.ProcessMetrics
	if err := json.Unmarshal(data, &sampled); err != nil {
		t.Fatal(err)
	}
	if len(sampled) != 1 || sampled[0].ID != processes[0].Config.ID {
		t.Fatalf("unexpected metrics: %#v", sampled)
	}

	missing := roundTrip(t, conn, scanner, 4, "process.get", map[string]string{"id": "missing"})
	if missing.Error == nil || missing.Error.Code != manager.CodeNotFound {
		t.Fatalf("error code was not preserved: %#v", missing.Error)
	}
}

func roundTrip(t *testing.T, conn net.Conn, scanner interface {
	Scan() bool
	Bytes() []byte
	Err() error
}, id int, method string, params any) protocol.Response {
	t.Helper()
	idJSON, _ := json.Marshal(id)
	paramsJSON, _ := json.Marshal(params)
	if params == nil {
		paramsJSON = nil
	}
	if err := protocol.WriteMessage(conn, protocol.Request{Protocol: protocol.Version, ID: idJSON, Method: method, Params: paramsJSON}); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatalf("no response: %v", scanner.Err())
	}
	var response protocol.Response
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
