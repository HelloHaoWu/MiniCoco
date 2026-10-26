package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

type sinkEntry struct {
	file string
	data []byte
}

// One bounded queue/writer for all structured record types. Diagnostics are
// best effort: dropping or failing a write never fails an agent round.
type jsonlSink struct {
	mu      sync.Mutex
	queue   chan sinkEntry
	done    chan struct{}
	closed  bool
	write   func(sinkEntry) error
	dropped atomic.Uint64
	failed  atomic.Uint64
}

func newJSONLSink(dir string, capacity int) *jsonlSink {
	s := &jsonlSink{queue: make(chan sinkEntry, capacity), done: make(chan struct{})}
	s.write = func(e sinkEntry) error {
		if strings.Contains(e.file, "\\") || filepath.IsAbs(e.file) || filepath.Clean(e.file) != e.file || strings.HasPrefix(e.file, "..") || !strings.HasSuffix(e.file, ".jsonl") {
			return ErrStoreInvalid
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, e.file)), 0700); err != nil {
			return err
		}
		path := filepath.Join(dir, e.file)
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return ErrStoreInvalid
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(e.data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	go func() {
		defer close(s.done)
		for e := range s.queue {
			if err := s.write(e); err != nil {
				s.failed.Add(1)
			}
		}
	}()
	return s
}

func (s *jsonlSink) emit(file string, record any) {
	if s == nil {
		return
	}
	b, err := json.Marshal(record)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- sinkEntry{file, b}:
		return
	default:
	}
	select {
	case <-s.queue:
		s.dropped.Add(1)
	default:
	}
	select {
	case s.queue <- sinkEntry{file, b}:
	default:
		s.dropped.Add(1)
	}
}

func (s *jsonlSink) close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
