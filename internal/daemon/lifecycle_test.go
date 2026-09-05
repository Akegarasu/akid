package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	akidlog "akid/internal/logging"
	"akid/internal/manager"
	"akid/internal/protocol"
	"akid/internal/storage"
)

func serverFixture(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	logs, err := akidlog.NewService(filepath.Join(dir, "logs"), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	mgr, err := manager.New(&storage.FileStore{Path: filepath.Join(dir, "state.json")}, noProcessExecutor{}, logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(listener, mgr, logs, func() {})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCapabilitiesAndEmptyApplyOverSocket(t *testing.T) {
	s := serverFixture(t)
	go func() { _ = s.Serve() }()
	conn, err := net.Dial("tcp", s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	scanner := protocol.NewScanner(conn)
	response := roundTrip(t, conn, scanner, 1, "daemon.capabilities", nil)
	data, _ := json.Marshal(response.Result)
	var caps protocol.Capabilities
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil || caps.Protocol != 2 || !slices.Contains(caps.Methods, "config.apply") || !slices.Contains(caps.Features, "process.snapshot") || caps.MaxMessageSize != protocol.MaxMessageSize {
		t.Fatalf("capabilities: %+v", response)
	}
	response = roundTrip(t, conn, scanner, 2, "config.apply", map[string]any{"processes": []any{}})
	if response.Error != nil {
		t.Fatalf("empty apply: %+v", response)
	}
	response = roundTrip(t, conn, scanner, 3, "config.apply", map[string]any{})
	if response.Error == nil || response.Error.Code != manager.CodeInvalidConfig {
		t.Fatalf("missing processes accepted: %+v", response)
	}
	response = roundTrip(t, conn, scanner, 4, "process.get", nil)
	if response.Error == nil || response.Error.Code != manager.CodeInvalidConfig {
		t.Fatalf("missing params accepted: %+v", response)
	}
	if err := protocol.WriteMessage(conn, protocol.Request{Protocol: 1, ID: json.RawMessage("5"), Method: "daemon.ping"}); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatal("missing version error")
	}
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "PROTOCOL_MISMATCH" {
		t.Fatalf("accepted old protocol: %+v", response)
	}
}

func TestServerCloseDisconnectsIdleControlAndSubscription(t *testing.T) {
	s := serverFixture(t)
	go func() { _ = s.Serve() }()
	var connections []net.Conn
	for _, method := range []string{"daemon.ping", "event.subscribe"} {
		conn, err := net.Dial("tcp", s.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		scanner := protocol.NewScanner(conn)
		if response := roundTrip(t, conn, scanner, 1, method, nil); response.Error != nil {
			t.Fatal(response.Error)
		}
		if method == "event.subscribe" && !scanner.Scan() {
			t.Fatal("missing snapshot")
		}
		connections = append(connections, conn)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, conn := range connections {
		var b [1]byte
		_, err := conn.Read(b[:])
		if err == nil {
			t.Fatal("connection stayed open")
		}
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("shutdown did not close connection")
		}
	}
}

func TestSlowSubscriberWriteTimesOut(t *testing.T) {
	s := serverFixture(t)
	s.writeTimeout = 30 * time.Millisecond
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { s.serveConn(server); close(done) }()
	if err := protocol.WriteMessage(client, protocol.Request{Protocol: 2, ID: json.RawMessage("1"), Method: "event.subscribe"}); err != nil {
		t.Fatal(err)
	}
	// Do not consume even the acknowledgement. The server must stop writing.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber retained its server goroutine")
	}
}
