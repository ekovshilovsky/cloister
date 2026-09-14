package vcsbroker

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

var responseWriteStallTimeout = 15 * time.Second

// responseSpool lets command execution write at disk speed while a separate
// reader streams available bytes to HTTP. A disconnected or half-open client
// therefore cannot block the host command or its post-command barrier.
type responseSpool struct {
	file    *os.File
	mu      sync.Mutex
	changed *sync.Cond
	written int64
	done    bool
	err     error
}

func newResponseSpool() (*responseSpool, error) {
	file, err := os.CreateTemp("", ".cloister-vcs-response-*")
	if err != nil {
		return nil, err
	}
	spool := &responseSpool{file: file}
	spool.changed = sync.NewCond(&spool.mu)
	return spool, nil
}

func (s *responseSpool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return 0, fmt.Errorf("VCS response spool is closed")
	}
	n, err := s.file.Write(p)
	s.written += int64(n)
	if err != nil {
		s.err = err
	}
	s.changed.Broadcast()
	return n, err
}

func (s *responseSpool) finish(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.done = true
	s.changed.Broadcast()
	s.mu.Unlock()
}

func (s *responseSpool) stream(w http.ResponseWriter) error {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32<<10)
	var offset int64
	for {
		s.mu.Lock()
		for offset == s.written && !s.done {
			s.changed.Wait()
		}
		end, done, spoolErr := s.written, s.done, s.err
		s.mu.Unlock()

		for offset < end {
			want := int64(len(buffer))
			if remaining := end - offset; remaining < want {
				want = remaining
			}
			n, err := s.file.ReadAt(buffer[:want], offset)
			if n > 0 {
				// This is a sliding bound on a blocked reader, not an absolute
				// command deadline. A client that keeps reading may receive a
				// long push's progress for as long as the command legitimately runs.
				_ = controller.SetWriteDeadline(time.Now().Add(responseWriteStallTimeout))
				written, writeErr := w.Write(buffer[:n])
				offset += int64(written)
				if writeErr != nil {
					return writeErr
				}
				if written != n {
					return fmt.Errorf("short VCS response write")
				}
			}
			if err != nil && n == 0 {
				return err
			}
		}
		if done && offset == end {
			_ = controller.SetWriteDeadline(time.Time{})
			return spoolErr
		}
	}
}

func (s *responseSpool) close() {
	if s == nil || s.file == nil {
		return
	}
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}
