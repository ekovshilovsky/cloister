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

// newStaleUnixSocket creates a socket *file* that no process is listening on,
// mimicking the host gpg-agent's socket inode after the agent has exited. The
// inode persists (gpg sockets live in ~/.gnupg, which survives reboots), so the
// file exists and reports os.ModeSocket, but dialing it is refused. Returns the
// socket path and a launch func that clears the stale file and binds a real
// listener, mimicking `gpgconf --launch gpg-agent` recreating its sockets.
func newStaleUnixSocket(t *testing.T) (string, func() error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cl-ehs-")
	if err != nil {
		t.Fatalf("creating socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	addr := &net.UnixAddr{Name: sock, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("creating socket fixture: %v", err)
	}
	// Keep the socket file on disk after Close so it stays a dangling inode.
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	launch := func() error {
		// gpg-agent unlinks the stale socket and rebinds; replicate that here.
		_ = os.Remove(sock)
		newLn, lerr := net.Listen("unix", sock)
		if lerr != nil {
			return lerr
		}
		t.Cleanup(func() { newLn.Close() })
		return nil
	}
	return sock, launch
}

// Regression for the reboot/idle case that survived the existence-only check:
// the socket FILE is present (passes os.Stat + ModeSocket) but nothing is
// listening, so the reverse tunnel forwards to a dead socket and the VM reports
// "End of file". ensureHostSocket must detect the dead socket via a dial probe,
// relaunch the agent, and succeed once the agent is actually listening.
func TestEnsureHostSocket_StaleSocketFile_RelaunchesAndConnects(t *testing.T) {
	sock, launch := newStaleUnixSocket(t)
	launched := false
	wrapped := func() error { launched = true; return launch() }

	if err := ensureHostSocket(sock, wrapped); err != nil {
		t.Fatalf("expected success after relaunch on stale socket, got: %v", err)
	}
	if !launched {
		t.Fatal("launcher must run when the socket file exists but is not listening")
	}
}

// A stale socket file that the launcher fails to revive must surface an error
// rather than silently starting a hollow tunnel.
func TestEnsureHostSocket_StaleSocketFile_LaunchNoOp_Errors(t *testing.T) {
	sock, _ := newStaleUnixSocket(t)
	err := ensureHostSocket(sock, func() error { return nil }) // launch does not revive it
	if err == nil {
		t.Fatal("expected error when stale socket is not revived by launch, got nil")
	}
	if !strings.Contains(err.Error(), "host socket") {
		t.Fatalf("error must mention host socket, got: %v", err)
	}
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
