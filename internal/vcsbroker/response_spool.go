package vcsbroker

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	responseSpoolPerCommandLimit = int64(16 << 20)
	responseSpoolAggregateLimit  = int64(64 << 20)
)

var responseWriteStallTimeout = 15 * time.Second

// responseSpoolManager bounds private response storage shared by one daemon.
type responseSpoolManager struct {
	dir             string
	removeDir       bool
	perCommandLimit int64
	aggregateLimit  int64

	mu       sync.Mutex
	reserved int64
}

func newResponseSpoolManager(dir string) (*responseSpoolManager, error) {
	removeDir := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", ".cloister-vcs-spools-")
		if err != nil {
			return nil, err
		}
		removeDir = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return nil, err
		}
	}
	return &responseSpoolManager{
		dir: dir, removeDir: removeDir,
		perCommandLimit: responseSpoolPerCommandLimit,
		aggregateLimit:  responseSpoolAggregateLimit,
	}, nil
}

func (m *responseSpoolManager) reserve(want int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	available := m.aggregateLimit - m.reserved
	if available < 0 {
		available = 0
	}
	if want > available {
		want = available
	}
	m.reserved += want
	return want
}

func (m *responseSpoolManager) release(size int64) {
	m.mu.Lock()
	m.reserved -= size
	if m.reserved < 0 {
		m.reserved = 0
	}
	m.mu.Unlock()
}

func (m *responseSpoolManager) close() {
	if m != nil && m.removeDir {
		_ = os.RemoveAll(m.dir)
	}
}

// responseSpool lets command execution write at disk speed while a separate
// reader streams available bytes to HTTP. Its directory entry is removed as
// soon as the file opens, which is supported by macOS and Linux: the open file
// remains usable but a daemon crash cannot leave command output named on disk.
type responseSpool struct {
	file      *os.File
	manager   *responseSpoolManager
	mu        sync.Mutex
	changed   *sync.Cond
	written   int64
	done      bool
	err       error
	truncated bool
}

func newResponseSpool(manager *responseSpoolManager) (*responseSpool, error) {
	file, err := os.CreateTemp(manager.dir, "response-")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		return nil, err
	}
	spool := &responseSpool{file: file, manager: manager}
	spool.changed = sync.NewCond(&spool.mu)
	return spool, nil
}

func (s *responseSpool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return 0, fmt.Errorf("VCS response spool is closed")
	}
	if s.truncated {
		return len(p), nil
	}
	remaining := s.manager.perCommandLimit - s.written
	if remaining < 0 {
		remaining = 0
	}
	want := int64(len(p))
	if want > remaining {
		want = remaining
	}
	reserved := s.manager.reserve(want)
	if reserved > 0 {
		n, err := s.file.Write(p[:reserved])
		s.written += int64(n)
		if int64(n) < reserved {
			s.manager.release(reserved - int64(n))
		}
		if err != nil || int64(n) != reserved {
			s.truncated = true
		}
	}
	if reserved < int64(len(p)) {
		s.truncated = true
	}
	s.changed.Broadcast()
	// Storage exhaustion must not block or fail the host command. The stream
	// reports truncation after execution and the post-command barrier finish.
	return len(p), nil
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
		end, done, spoolErr, truncated := s.written, s.done, s.err, s.truncated
		s.mu.Unlock()

		for offset < end {
			want := int64(len(buffer))
			if remaining := end - offset; remaining < want {
				want = remaining
			}
			n, err := s.file.ReadAt(buffer[:want], offset)
			if n > 0 {
				written, writeErr := writeVCSResponse(controller, w, buffer[:n])
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
			if truncated {
				notice := fmt.Sprintf("\ncloister: VCS broker output truncated after %d bytes (per-command limit %d bytes; daemon aggregate limit %d bytes); the command and synchronization barrier completed\n", end, s.manager.perCommandLimit, s.manager.aggregateLimit)
				if _, err := writeVCSResponse(controller, w, []byte(notice)); err != nil {
					return err
				}
			}
			_ = controller.SetWriteDeadline(time.Time{})
			return spoolErr
		}
	}
}

func writeVCSResponse(controller *http.ResponseController, w http.ResponseWriter, data []byte) (int, error) {
	// The deadline slides on every write. A slow client that keeps making
	// progress can receive an arbitrarily long response, while a stalled reader
	// cannot retain a delivery slot past the stall bound.
	_ = controller.SetWriteDeadline(time.Now().Add(responseWriteStallTimeout))
	return w.Write(data)
}

func (s *responseSpool) close() {
	if s == nil || s.file == nil {
		return
	}
	_ = s.file.Close()
	s.manager.release(s.written)
}
