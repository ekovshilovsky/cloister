package vcsbroker

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponseSpoolIsPrivateUnlinkedAndCleansCrashLeftovers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spools")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale-secret")
	if err := os.WriteFile(stale, []byte("credential marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := newResponseSpoolManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("spool directory mode=%v", info.Mode().Perm())
	}
	spool, err := newResponseSpool(manager)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if _, err := spool.Write([]byte("new credential marker")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("named spool entries=%v error=%v", entries, err)
	}
}

func TestResponseSpoolCapsDiscardOutputWithoutFailingCommand(t *testing.T) {
	manager, err := newResponseSpoolManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.perCommandLimit = 10
	manager.aggregateLimit = 12
	first, _ := newResponseSpool(manager)
	second, _ := newResponseSpool(manager)
	defer first.close()
	defer second.close()
	if n, err := first.Write([]byte("0123456789ABC")); n != 13 || err != nil {
		t.Fatalf("first write=(%d,%v)", n, err)
	}
	if n, err := second.Write([]byte("uvwxyz")); n != 6 || err != nil {
		t.Fatalf("second write=(%d,%v)", n, err)
	}
	first.finish(nil)
	second.finish(nil)
	response := httptest.NewRecorder()
	if err := first.stream(response); err != nil {
		t.Fatal(err)
	}
	if body := response.Body.String(); !strings.HasPrefix(body, "0123456789") || !strings.Contains(body, "output truncated after 10 bytes") || !strings.Contains(body, "command and synchronization barrier completed") {
		t.Fatalf("first response=%q", body)
	}
	response = httptest.NewRecorder()
	if err := second.stream(response); err != nil {
		t.Fatal(err)
	}
	if body := response.Body.String(); !strings.HasPrefix(body, "uv") || !strings.Contains(body, "output truncated after 2 bytes") {
		t.Fatalf("aggregate-capped response=%q", body)
	}
}

type progressingResponseWriter struct {
	header   http.Header
	deadline time.Time
	body     bytes.Buffer
}

func (w *progressingResponseWriter) Header() http.Header { return w.header }
func (w *progressingResponseWriter) WriteHeader(int)     {}
func (w *progressingResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *progressingResponseWriter) Write(data []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	if time.Now().After(w.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return w.body.Write(data)
}

func TestResponseSpoolWriteDeadlineSlidesForProgressingReader(t *testing.T) {
	previous := responseWriteStallTimeout
	responseWriteStallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { responseWriteStallTimeout = previous })
	manager, err := newResponseSpoolManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spool, err := newResponseSpool(manager)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	payload := bytes.Repeat([]byte("x"), 96<<10)
	if _, err := spool.Write(payload); err != nil {
		t.Fatal(err)
	}
	spool.finish(nil)
	writer := &progressingResponseWriter{header: make(http.Header)}
	started := time.Now()
	if err := spool.stream(writer); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) <= responseWriteStallTimeout {
		t.Fatal("test response did not span the per-write deadline")
	}
	if !bytes.Equal(writer.body.Bytes(), payload) {
		t.Fatalf("received %d bytes, want %d", writer.body.Len(), len(payload))
	}
}
