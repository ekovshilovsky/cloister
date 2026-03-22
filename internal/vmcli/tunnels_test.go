package vmcli

import (
	"net"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
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
