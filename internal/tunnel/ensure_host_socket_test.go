package tunnel

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newUnixSocket creates a real Unix-domain socket and returns its path. Darwin
// enforces a ~104-byte limit on sun_path, so the socket lives under
// os.TempDir() rather than the much longer t.TempDir() path.
func newUnixSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("creating socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("creating unix socket fixture: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return sock
}

// When the host socket is already present, ensureHostSocket must succeed
// without invoking the launcher — the host gpg-agent is already up.
func TestEnsureHostSocket_PresentSocket_DoesNotLaunch(t *testing.T) {
	sock := newUnixSocket(t)
	launched := false
	err := ensureHostSocket(sock, func() error { launched = true; return nil })
	if err != nil {
		t.Fatalf("expected nil error for live socket, got: %v", err)
	}
	if launched {
		t.Fatal("launcher must not run when the socket already exists")
	}
}

// The recurrence fix: a missing socket (host gpg-agent not running) must
// trigger the launcher, after which the socket exists and the call succeeds.
func TestEnsureHostSocket_MissingThenLaunchCreatesIt(t *testing.T) {
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	launched := false
	launch := func() error {
		launched = true
		ln, lerr := net.Listen("unix", sock)
		if lerr != nil {
			return lerr
		}
		t.Cleanup(func() { ln.Close() })
		return nil
	}

	if err := ensureHostSocket(sock, launch); err != nil {
		t.Fatalf("expected success after launch created the socket, got: %v", err)
	}
	if !launched {
		t.Fatal("launcher must run when the socket is initially missing")
	}
}

// If the socket is still absent after the launch attempt, the error must name
// the host socket so the caller can surface an actionable warning.
func TestEnsureHostSocket_MissingAfterLaunch_Errors(t *testing.T) {
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	err = ensureHostSocket(sock, func() error { return nil }) // launch is a no-op
	if err == nil {
		t.Fatal("expected error when socket absent after launch, got nil")
	}
	if !strings.Contains(err.Error(), "host socket") {
		t.Fatalf("error must mention host socket, got: %v", err)
	}
}

// A launcher that itself fails must surface that failure.
func TestEnsureHostSocket_LaunchFails_Errors(t *testing.T) {
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	err = ensureHostSocket(sock, func() error { return fmt.Errorf("gpgconf boom") })
	if err == nil {
		t.Fatal("expected error when launcher fails, got nil")
	}
}

// A path that exists but is a regular file (not a socket) is a misconfiguration
// the launcher cannot fix, so it must error without launching.
func TestEnsureHostSocket_PathNotSocket_Errors(t *testing.T) {
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	regular := filepath.Join(dir, "f")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing regular file: %v", err)
	}

	launched := false
	err = ensureHostSocket(regular, func() error { launched = true; return nil })
	if err == nil {
		t.Fatal("expected error when path is not a socket, got nil")
	}
	if launched {
		t.Fatal("launcher must not run when the path exists but is not a socket")
	}
}
