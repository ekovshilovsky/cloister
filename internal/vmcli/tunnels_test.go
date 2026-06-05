package vmcli

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cloister.io/internal/vmconfig"
)

func TestCheckTunnels(t *testing.T) {
	// Start a test listener to simulate an available tunnel
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	tunnels := []vmconfig.TunnelDef{
		{Name: "available", Port: port},
		{Name: "unavailable", Port: 59999},
	}

	results := CheckTunnels(tunnels, 100)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if !results[0].Connected {
		t.Error("available tunnel should be connected")
	}
	if results[1].Connected {
		t.Error("unavailable tunnel should not be connected")
	}
}

func TestTunnelResultFormat(t *testing.T) {
	r := TunnelResult{
		Name:      "clipboard",
		Port:      18339,
		Connected: true,
	}
	s := r.String()
	if s == "" {
		t.Error("String() should produce output")
	}
}

// TestCheckTunnelsSocket verifies that CheckTunnels probes a socket tunnel
// by stat-ing the resolved Unix-socket path (with $UID substitution),
// rather than TCP-probing port 0. A real Unix socket is created in a temp
// dir using net.Listen("unix", ...) so the probe sees a genuine socket
// file; the test then asserts both the connected branch (path matches a
// socket) and the disconnected branch (path is absent).
func TestCheckTunnelsSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "cl-tunnel-sock-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)

	sockPath := filepath.Join(dir, "S.gpg-agent")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	tunnels := []vmconfig.TunnelDef{
		{Name: "gpg-forward", Socket: sockPath},
		{Name: "missing-socket", Socket: filepath.Join(dir, "does-not-exist")},
	}

	results := CheckTunnels(tunnels, 100)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if !results[0].Connected {
		t.Errorf("real socket should be reported as connected; got %+v", results[0])
	}
	if results[0].Socket != sockPath {
		t.Errorf("Socket field should round-trip resolved path; got %q want %q", results[0].Socket, sockPath)
	}
	if results[0].Port != 0 {
		t.Errorf("socket tunnel should have Port=0 in result; got %d", results[0].Port)
	}

	if results[1].Connected {
		t.Errorf("missing socket path should be reported as not connected; got %+v", results[1])
	}
}

// TestCheckTunnelsSocketUIDSubstitution verifies the $UID placeholder in
// the Socket field is substituted against os.Getuid() before stat-ing.
// Using a path that contains "$UID" but resolves to an unreachable
// directory under the test's UID confirms the substitution path is
// exercised even when the file does not exist.
func TestCheckTunnelsSocketUIDSubstitution(t *testing.T) {
	tunnels := []vmconfig.TunnelDef{
		{Name: "gpg-forward", Socket: "/run/user/$UID/gnupg/S.gpg-agent"},
	}
	results := CheckTunnels(tunnels, 100)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	uidStr := strconv.Itoa(os.Getuid())
	if !strings.Contains(results[0].Socket, "/run/user/"+uidStr+"/gnupg/") {
		t.Errorf("Socket field should have $UID substituted to %s; got %q", uidStr, results[0].Socket)
	}
	if strings.Contains(results[0].Socket, "$UID") {
		t.Errorf("Socket field still contains literal $UID after substitution: %q", results[0].Socket)
	}
}
