package logging

import (
	"context"
	"os"
	"testing"
	"time"

	"akid/internal/model"
)

func TestReadAlignsToCompleteLines(t *testing.T) {
	service, err := NewService(t.TempDir(), 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	path := service.Path("api", model.LogStdout)
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	chunk, err := service.Read(LogReadRequest{Name: "api", Stream: model.LogStdout, Offset: 2, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if chunk.StartOffset != 6 || string(chunk.Data) != "second\nthird\n" || !chunk.EOF {
		t.Fatalf("unexpected aligned chunk: %#v data=%q", chunk, chunk.Data)
	}

	chunk, err = service.Read(LogReadRequest{Name: "api", Stream: model.LogStdout, Offset: 0, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk.Data) != "first\n" || chunk.EndOffset != 6 || chunk.EOF {
		t.Fatalf("unexpected limited chunk: %#v data=%q", chunk, chunk.Data)
	}
}

func TestReadBuffersUnterminatedFinalLine(t *testing.T) {
	service, err := NewService(t.TempDir(), 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.Path("api", model.LogStdout), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	chunk, err := service.Read(LogReadRequest{Name: "api", Stream: model.LogStdout, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk.Data) != 0 || chunk.EndOffset != 0 || chunk.EOF {
		t.Fatalf("unterminated line should remain buffered: %#v", chunk)
	}
}

func TestRotateCopyTruncateAndGeneration(t *testing.T) {
	service, err := NewService(t.TempDir(), 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	path := service.Path("api", model.LogStdout)
	content := []byte("line1\nline2\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.rotate("api", model.LogStdout); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != string(content) {
		t.Fatalf("rotated content = %q", rotated)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || service.Generation("api", model.LogStdout) != 1 {
		t.Fatalf("rotation did not truncate/increment generation: size=%d generation=%d", info.Size(), service.Generation("api", model.LogStdout))
	}
}

func TestRotatingWriter(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	writer, err := NewRotatingWriter(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "first\n" || string(active) != "second\n" {
		t.Fatalf("unexpected files: rotated=%q active=%q", rotated, active)
	}
}

func TestSubscribeFollowsAppends(t *testing.T) {
	service, err := NewService(t.TempDir(), 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := service.Subscribe(ctx, "api", model.LogStdout, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(service.Path("api", model.LogStdout), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	select {
	case event := <-events:
		if event.Lagged || string(event.Chunk.Data) != "hello\n" {
			t.Fatalf("unexpected event: %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for append")
	}
}
