package logging

import (
	"bytes"
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

	chunk, err := service.Read(LogReadRequest{Name: "api", Stream: model.LogStdout, Offset: 2, Limit: 100, Align: true})
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

func TestReadReturnsUnterminatedFinalLine(t *testing.T) {
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
	if string(chunk.Data) != "partial" || chunk.EndOffset != 7 || !chunk.EOF || !chunk.PartialEnd {
		t.Fatalf("unterminated line must remain readable: %#v", chunk)
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
	generation := service.Generation("api", model.LogStdout)
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
	if info.Size() != 0 || service.Generation("api", model.LogStdout) == generation {
		t.Fatalf("rotation did not truncate/change generation: size=%d generation=%d", info.Size(), service.Generation("api", model.LogStdout))
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
	events, err := service.Subscribe(ctx, "api", model.LogStdout, 0, service.Generation("api", model.LogStdout))
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

func TestReadLongLineContinuationsAreLossless(t *testing.T) {
	service, err := NewService(t.TempDir(), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	data := append(bytes.Repeat([]byte("x"), (64<<10)+13), []byte("\nlast without newline")...)
	if err := os.WriteFile(service.Path("api", model.LogStdout), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var got []byte
	var cursor int64
	for i := 0; i < 10; i++ {
		chunk, err := service.Read(LogReadRequest{Name: "api", Stream: model.LogStdout, Offset: cursor, Limit: 64 << 10})
		if err != nil {
			t.Fatal(err)
		}
		if chunk.StartOffset != cursor {
			t.Fatalf("skipped bytes: %d -> %d", cursor, chunk.StartOffset)
		}
		got = append(got, chunk.Data...)
		cursor = chunk.EndOffset
		if chunk.EOF {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d of %d bytes", len(got), len(data))
	}
}

func TestSubscriptionReportsGapAfterServiceRestart(t *testing.T) {
	dir := t.TempDir()
	old, err := NewService(dir, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Register("api"); err != nil {
		t.Fatal(err)
	}
	generation := old.Generation("api", model.LogStdout)
	old.Close()
	service, err := NewService(dir, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := service.Subscribe(ctx, "api", model.LogStdout, 500, generation)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if !event.Gap || event.Chunk.Generation == generation {
			t.Fatalf("missing restart gap: %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("no gap for empty active file")
	}
}

func TestRemoveChecksOwnerAndDetachesOldSubscriptions(t *testing.T) {
	service, err := NewService(t.TempDir(), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Register("api", "old"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := service.Subscribe(ctx, "api", model.LogStdout, 0, service.Generation("api", model.LogStdout))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Remove("api", "wrong", true); err == nil {
		t.Fatal("accepted wrong owner")
	}
	if err := service.Remove("api", "old", true); err != nil {
		t.Fatal(err)
	}
	if err := service.Register("api", "new"); err != nil {
		t.Fatal(err)
	}
	path := service.Path("api", model.LogStdout)
	if err := os.WriteFile(path, []byte("new process\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove("api", "old", true); err == nil {
		t.Fatal("old cleanup deleted new process logs")
	}
	select {
	case event, open := <-events:
		if open {
			t.Fatalf("old subscriber followed new process: %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("old subscription did not close")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new process\n" {
		t.Fatalf("new logs changed: %q %v", data, err)
	}
}

func TestReadAndSubscribeRejectPreviousOwner(t *testing.T) {
	s, err := NewService(t.TempDir(), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Register("api", "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("api", "old", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Register("api", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(LogReadRequest{Name: "api", Owner: "old", Stream: model.LogStdout}); !os.IsNotExist(err) {
		t.Fatalf("old read: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Subscribe(ctx, "api", model.LogStdout, 0, 0, "old"); !os.IsNotExist(err) {
		t.Fatalf("old subscription: %v", err)
	}
	if _, err := s.Read(LogReadRequest{Name: "api", Owner: "new", Stream: model.LogStdout}); err != nil {
		t.Fatalf("new read: %v", err)
	}
}

func TestSubscriptionReportsGapForCursorBeyondEOF(t *testing.T) {
	s, err := NewService(t.TempDir(), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Register("api"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path("api", model.LogStdout), []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	gen := s.Generation("api", model.LogStdout)
	events, err := s.Subscribe(ctx, "api", model.LogStdout, 500, gen)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if !event.Gap || event.Chunk.Generation != gen {
			t.Fatalf("missing gap: %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("no gap")
	}
	select {
	case event := <-events:
		if string(event.Chunk.Data) != "current\n" || event.Chunk.StartOffset != 0 {
			t.Fatalf("wrong recovery: %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("no replay after gap")
	}
}
