package logging

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
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

var ErrInvalidRequest = errors.New("invalid log request")

type LogReadRequest struct {
	Name   string
	Owner  string
	Stream model.LogStream
	Offset int64
	Limit  int
	Align  bool
}

type LogChunk struct {
	StartOffset  int64  `json:"start_offset"`
	EndOffset    int64  `json:"end_offset"`
	Generation   uint64 `json:"generation"`
	Data         []byte `json:"data"`
	EOF          bool   `json:"eof"`
	PartialStart bool   `json:"partial_start,omitempty"`
	PartialEnd   bool   `json:"partial_end,omitempty"`
}

type LogEvent struct {
	Name   string          `json:"name,omitempty"`
	Stream model.LogStream `json:"stream,omitempty"`
	Chunk  LogChunk        `json:"chunk,omitempty"`
	Lagged bool            `json:"lagged,omitempty"`
	Gap    bool            `json:"gap,omitempty"`
}

type fileState struct {
	mu         sync.Mutex
	generation uint64
	removed    bool
}

type processLogs struct {
	owner  string
	stdout fileState
	stderr fileState
}

type Service struct {
	dir          string
	maxSize      int64
	keep         int
	mu           sync.RWMutex
	entries      map[string]*processLogs
	closed       chan struct{}
	closeOne     sync.Once
	rotationDone chan struct{}
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
		dir:          dir,
		maxSize:      maxSize,
		keep:         keep,
		entries:      make(map[string]*processLogs),
		closed:       make(chan struct{}),
		rotationDone: make(chan struct{}),
	}
	go s.rotationLoop()
	return s, nil
}

func (s *Service) Register(name string, owners ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := ""
	if len(owners) > 0 {
		owner = owners[0]
	}
	if entry := s.entries[name]; entry != nil {
		if entry.owner != owner {
			return errors.New("log name belongs to another process")
		}
		return nil
	}
	outGeneration, err := newGeneration()
	if err != nil {
		return err
	}
	errGeneration, err := newGeneration()
	if err != nil {
		return err
	}
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
	s.entries[name] = &processLogs{owner: owner, stdout: fileState{generation: outGeneration}, stderr: fileState{generation: errGeneration}}
	return nil
}

func newGeneration() (uint64, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return 0, err
	}
	// Stay exactly representable by JSON consumers using IEEE-754 numbers.
	return (binary.BigEndian.Uint64(data[:]) >> 12) + 1, nil
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
		return LogChunk{}, fmt.Errorf("%w: stream must be stdout or stderr", ErrInvalidRequest)
	}
	if req.Limit <= 0 {
		req.Limit = 64 << 10
	}
	if req.Limit > MaxReadSize {
		req.Limit = MaxReadSize
	}
	state := s.file(req.Name, req.Stream, req.Owner)
	return s.read(req, state)
}

func (s *Service) read(req LogReadRequest, state *fileState) (LogChunk, error) {
	if state == nil {
		return LogChunk{}, os.ErrNotExist
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.removed {
		return LogChunk{}, os.ErrNotExist
	}

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
	start := offset
	if req.Align || req.Offset < 0 {
		start, err = alignStart(f, offset, min(size, offset+int64(req.Limit)))
		if err != nil {
			return LogChunk{}, err
		}
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
	if last := bytes.LastIndexByte(buf, '\n'); last >= 0 && last+1 < len(buf) && end < size {
		buf = buf[:last+1]
		end = start + int64(last+1)
	}
	partialStart := false
	if start > 0 {
		var previous [1]byte
		if _, err := f.ReadAt(previous[:], start-1); err != nil {
			return LogChunk{}, err
		}
		partialStart = previous[0] != '\n'
	}
	return LogChunk{
		StartOffset:  start,
		EndOffset:    end,
		Generation:   state.generation,
		Data:         buf,
		EOF:          end >= size,
		PartialStart: partialStart,
		PartialEnd:   len(buf) > 0 && buf[len(buf)-1] != '\n',
	}, nil
}

func (s *Service) Subscribe(ctx context.Context, name string, stream model.LogStream, offset int64, generation uint64, owners ...string) (<-chan LogEvent, error) {
	if stream != model.LogStdout && stream != model.LogStderr || offset < 0 {
		return nil, fmt.Errorf("%w: stream must be stdout or stderr and subscription offset must be nonnegative", ErrInvalidRequest)
	}
	state := s.file(name, stream, owners...)
	if state == nil {
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
					select {
					case <-ctx.Done():
						return
					case <-s.closed:
						return
					default:
					}
					chunk, err := s.read(LogReadRequest{Name: name, Stream: stream, Offset: cursor, Limit: 64 << 10}, state)
					if err != nil {
						return
					}
					if chunk.Generation != currentGeneration || chunk.StartOffset != cursor {
						currentGeneration = chunk.Generation
						cursor = 0
						// Archives are not part of this stream. Explicitly invalidate
						// the cursor even when the new active file is still empty.
						if !sendLogEvent(out, LogEvent{Name: name, Stream: stream, Gap: true, Chunk: LogChunk{Generation: currentGeneration}}) {
							return
						}
						continue
					}
					if len(chunk.Data) == 0 {
						break
					}
					cursor = chunk.EndOffset
					event := LogEvent{Name: name, Stream: stream, Chunk: chunk}
					if !sendLogEvent(out, event) {
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

func sendLogEvent(out chan LogEvent, event LogEvent) bool {
	select {
	case out <- event:
		return true
	default:
		select {
		case <-out:
		default:
		}
		out <- LogEvent{Lagged: true}
		return false
	}
}

func (s *Service) Remove(name, owner string, purge bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[name]
	if entry == nil {
		return nil
	}
	if entry.owner != owner {
		return errors.New("log owner mismatch")
	}
	entry.stdout.mu.Lock()
	defer entry.stdout.mu.Unlock()
	entry.stderr.mu.Lock()
	defer entry.stderr.mu.Unlock()
	var first error
	if purge {
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
	}
	if first == nil {
		entry.stdout.removed, entry.stderr.removed = true, true
		delete(s.entries, name)
	}
	return first
}

func (s *Service) Close() error {
	s.closeOne.Do(func() { close(s.closed) })
	<-s.rotationDone
	return nil
}

func (s *Service) rotationLoop() {
	defer close(s.rotationDone)
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
	if state.removed {
		return nil
	}
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
	generation, err := newGeneration()
	if err != nil {
		return err
	}
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	state.generation = generation
	return nil
}

func (s *Service) file(name string, stream model.LogStream, owners ...string) *fileState {
	s.mu.RLock()
	entry := s.entries[name]
	s.mu.RUnlock()
	if entry == nil {
		return nil
	}
	if len(owners) > 0 && owners[0] != "" && owners[0] != entry.owner {
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
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-cursor)], cursor)
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
	// A long or unterminated line must still be readable from this offset.
	return offset, nil
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
