package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"akid/internal/model"
)

const (
	DefaultMaxSize = int64(20 << 20)
	DefaultKeep    = 5
	MaxReadSize    = 8 << 20
)

type LogReadRequest struct {
	Name   string
	Stream model.LogStream
	Offset int64
	Limit  int
}

type LogChunk struct {
	StartOffset int64  `json:"start_offset"`
	EndOffset   int64  `json:"end_offset"`
	Generation  uint64 `json:"generation"`
	Data        []byte `json:"data"`
	EOF         bool   `json:"eof"`
}

type LogEvent struct {
	Name   string          `json:"name,omitempty"`
	Stream model.LogStream `json:"stream,omitempty"`
	Chunk  LogChunk        `json:"chunk,omitempty"`
	Lagged bool            `json:"lagged,omitempty"`
}

type fileState struct {
	mu         sync.Mutex
	generation uint64
}

type processLogs struct {
	stdout fileState
	stderr fileState
}

type Service struct {
	dir      string
	maxSize  int64
	keep     int
	mu       sync.RWMutex
	entries  map[string]*processLogs
	closed   chan struct{}
	closeOne sync.Once
}

func NewService(dir string, maxSize int64, keep int) (*Service, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if keep < 0 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cleanupRotationTemps(dir)
	s := &Service{
		dir:     dir,
		maxSize: maxSize,
		keep:    keep,
		entries: make(map[string]*processLogs),
		closed:  make(chan struct{}),
	}
	go s.rotationLoop()
	return s, nil
}

func (s *Service) Register(name string) error {
	s.mu.Lock()
	if _, ok := s.entries[name]; !ok {
		s.entries[name] = &processLogs{}
	}
	s.mu.Unlock()
	for _, stream := range []model.LogStream{model.LogStdout, model.LogStderr} {
		f, err := os.OpenFile(s.Path(name, stream), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Path(name string, stream model.LogStream) string {
	suffix := "out"
	if stream == model.LogStderr {
		suffix = "err"
	}
	return filepath.Join(s.dir, name+"."+suffix+".log")
}

func (s *Service) Paths(name string) (stdout, stderr string) {
	return s.Path(name, model.LogStdout), s.Path(name, model.LogStderr)
}

func (s *Service) Generation(name string, stream model.LogStream) uint64 {
	state := s.file(name, stream)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.generation
}

func (s *Service) Read(req LogReadRequest) (LogChunk, error) {
	if req.Stream != model.LogStdout && req.Stream != model.LogStderr {
		return LogChunk{}, errors.New("invalid log stream")
	}
	if req.Limit <= 0 {
		req.Limit = 64 << 10
	}
	if req.Limit > MaxReadSize {
		req.Limit = MaxReadSize
	}
	state := s.file(req.Name, req.Stream)
	if state == nil {
		return LogChunk{}, os.ErrNotExist
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	f, err := os.Open(s.Path(req.Name, req.Stream))
	if err != nil {
		return LogChunk{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return LogChunk{}, err
	}
	size := info.Size()
	offset := req.Offset
	if offset < 0 {
		offset = size + offset
	}
	if offset < 0 {
		offset = 0
	}
	if offset > size {
		offset = size
	}
	start, err := alignStart(f, offset, size)
	if err != nil {
		return LogChunk{}, err
	}
	if start >= size {
		return LogChunk{StartOffset: start, EndOffset: start, Generation: state.generation, EOF: true}, nil
	}

	buf := make([]byte, req.Limit)
	n, readErr := f.ReadAt(buf, start)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return LogChunk{}, readErr
	}
	buf = buf[:n]
	end := start + int64(n)
	reachedEOF := end >= size
	if last := bytes.LastIndexByte(buf, '\n'); last >= 0 && last+1 < len(buf) {
		buf = buf[:last+1]
		end = start + int64(last+1)
	} else if last < 0 && reachedEOF {
		// Keep an unterminated final line buffered until it becomes complete.
		// This is important for follow mode: advancing into a partial line
		// would make the next aligned read skip its continuation.
		buf = buf[:0]
		end = start
	}
	// If a single complete line is longer than Limit, return a partial chunk
	// so callers still make progress. Normal lines remain line-aligned.
	return LogChunk{
		StartOffset: start,
		EndOffset:   end,
		Generation:  state.generation,
		Data:        buf,
		EOF:         end >= size,
	}, nil
}

func (s *Service) Subscribe(ctx context.Context, name string, stream model.LogStream, offset int64, generation uint64) (<-chan LogEvent, error) {
	if s.file(name, stream) == nil {
		return nil, os.ErrNotExist
	}
	out := make(chan LogEvent, 1024)
	go func() {
		defer close(out)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		cursor := offset
		currentGeneration := generation
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.closed:
				return
			case <-ticker.C:
				for {
					chunk, err := s.Read(LogReadRequest{Name: name, Stream: stream, Offset: cursor, Limit: 64 << 10})
					if err != nil {
						return
					}
					if chunk.Generation != currentGeneration {
						currentGeneration = chunk.Generation
						cursor = 0
						continue
					}
					if len(chunk.Data) == 0 {
						break
					}
					cursor = chunk.EndOffset
					event := LogEvent{Name: name, Stream: stream, Chunk: chunk}
					select {
					case out <- event:
					default:
						select {
						case <-out:
						default:
						}
						out <- LogEvent{Lagged: true}
						return
					}
					if chunk.EOF {
						break
					}
				}
			}
		}
	}()
	return out, nil
}

func (s *Service) Purge(name string) error {
	s.mu.Lock()
	delete(s.entries, name)
	s.mu.Unlock()
	var first error
	for _, stream := range []model.LogStream{model.LogStdout, model.LogStderr} {
		path := s.Path(name, stream)
		for i := 0; i <= s.keep; i++ {
			candidate := path
			if i > 0 {
				candidate = fmt.Sprintf("%s.%d", path, i)
			}
			if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *Service) Close() error {
	s.closeOne.Do(func() { close(s.closed) })
	return nil
}

func (s *Service) rotationLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			s.rotateAll()
		}
	}
}

func (s *Service) rotateAll() {
	s.mu.RLock()
	names := make([]string, 0, len(s.entries))
	for name := range s.entries {
		names = append(names, name)
	}
	s.mu.RUnlock()
	for _, name := range names {
		for _, stream := range []model.LogStream{model.LogStdout, model.LogStderr} {
			_ = s.rotate(name, stream)
		}
	}
}

func (s *Service) rotate(name string, stream model.LogStream) error {
	state := s.file(name, stream)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	path := s.Path(name, stream)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size() <= s.maxSize {
		return nil
	}
	if s.keep > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", path, s.keep))
		for i := s.keep - 1; i >= 1; i-- {
			oldPath := fmt.Sprintf("%s.%d", path, i)
			newPath := fmt.Sprintf("%s.%d", path, i+1)
			if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		tmp := path + ".rotate.tmp"
		if err := copyFile(path, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, path+".1"); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	state.generation++
	return nil
}

func (s *Service) file(name string, stream model.LogStream) *fileState {
	s.mu.RLock()
	entry := s.entries[name]
	s.mu.RUnlock()
	if entry == nil {
		return nil
	}
	if stream == model.LogStderr {
		return &entry.stderr
	}
	if stream == model.LogStdout {
		return &entry.stdout
	}
	return nil
}

func alignStart(f *os.File, offset, size int64) (int64, error) {
	if offset <= 0 || offset >= size {
		return offset, nil
	}
	var previous [1]byte
	if _, err := f.ReadAt(previous[:], offset-1); err != nil {
		return 0, err
	}
	if previous[0] == '\n' {
		return offset, nil
	}
	buf := make([]byte, 4096)
	cursor := offset
	for cursor < size {
		n, err := f.ReadAt(buf, cursor)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return cursor + int64(i+1), nil
		}
		cursor += int64(n)
		if n == 0 {
			break
		}
	}
	return size, nil
}

func cleanupRotationTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.rotate.tmp"))
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = dst.Close()
		if !ok {
			_ = os.Remove(to)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
