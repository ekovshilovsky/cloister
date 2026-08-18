package tunnel

import (
	"net"
	"testing"

	"cloister.io/internal/config"
)

// TestAgentGridPortsDiffer guards the core reason this forward exists: the Mac
// desktop app binds *:8765 and Lima auto-forwards guest ports to the host on
// the same port number, so the host side must not reuse the guest port.
func TestAgentGridPortsDiffer(t *testing.T) {
	if AgentGridDaemonPort != 8765 {
		t.Errorf("AgentGridDaemonPort = %d, want 8765", AgentGridDaemonPort)
	}
	if AgentGridHostPort == AgentGridDaemonPort {
		t.Fatalf("host port must differ from the guest port; both are %d", AgentGridHostPort)
	}
}

// TestLocalForwardPortAbsentProfile confirms the reporting helper treats an
// unknown profile as "no live forward" rather than returning a bogus port.
func TestLocalForwardPortAbsentProfile(t *testing.T) {
	port, ok := LocalForwardPort("cloister-test-nonexistent-profile", "agentgrid")
	if ok {
		t.Errorf("expected no recorded forward, got port %d", port)
	}
}

func TestParseAgentGridPairingCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "standard output",
			output: "Pairing code: A7B9CZ\nExpires in ~5m.\n",
			want:   "A7B9CZ",
		},
		{
			name:   "ignores unrelated output",
			output: "[daemon] ready\nPairing code: 123ABC\n",
			want:   "123ABC",
		},
		{
			name:   "missing code",
			output: "daemon is running\n",
			want:   "",
		},
		{
			name:   "rejects malformed code",
			output: "Pairing code: ABC\n",
			want:   "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseAgentGridPairingCode(tt.output); got != tt.want {
				t.Fatalf("parseAgentGridPairingCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStartLocalForwardSkipsBoundPort verifies the host-port search walks past
// a port that is already bound instead of failing. The occupied port stands in
// for the Mac's own Agent Grid daemon.
func TestStartLocalForwardSkipsBoundPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback in this environment: %v", err)
	}
	defer ln.Close()

	occupied := ln.Addr().(*net.TCPAddr).Port

	got, err := findFreeHostPort(occupied, 10)
	if err != nil {
		t.Fatalf("findFreeHostPort(%d): %v", occupied, err)
	}
	if got == occupied {
		t.Fatalf("findFreeHostPort returned occupied port %d", occupied)
	}
}

// TestFindFreeHostPortExcludingSkipsReserved verifies the allocator treats a
// reserved port as unavailable even when it would currently accept a bind, so
// a second profile never lands on a port another profile pinned.
func TestFindFreeHostPortExcludingSkipsReserved(t *testing.T) {
	base := 41000
	got, err := findFreeHostPortExcluding(base, 10, map[int]bool{base: true, base + 1: true})
	if err != nil {
		t.Fatalf("findFreeHostPortExcluding: %v", err)
	}
	if got == base || got == base+1 {
		t.Fatalf("allocator returned excluded port %d", got)
	}
}

// TestReserveHostPortPinsAndPersists confirms the first allocation records the
// chosen port in the profile config and rewrites config.yaml.
func TestReserveHostPortPinsAndPersists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {Stacks: []string{"agentgrid"}},
	}}

	port, err := reserveHostPort(cfgPath, cfg, "innolumi", "agentgrid", 41100)
	if err != nil {
		t.Fatalf("reserveHostPort: %v", err)
	}
	if port < 41100 || port >= 41100+hostPortScanLimit {
		t.Fatalf("port %d outside expected scan window", port)
	}
	if cfg.Profiles["innolumi"].LocalForwardPorts["agentgrid"] != port {
		t.Fatalf("in-memory reservation not set")
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := reloaded.Profiles["innolumi"].LocalForwardPorts["agentgrid"]; got != port {
		t.Fatalf("persisted reservation = %d, want %d", got, port)
	}
}

// TestReserveHostPortReusesPin confirms an existing reservation is returned
// verbatim without rescanning or moving the endpoint.
func TestReserveHostPortReusesPin(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {
			Stacks:            []string{"agentgrid"},
			LocalForwardPorts: map[string]int{"agentgrid": 18790},
		},
	}}

	port, err := reserveHostPort(cfgPath, cfg, "innolumi", "agentgrid", AgentGridHostPort)
	if err != nil {
		t.Fatalf("reserveHostPort: %v", err)
	}
	if port != 18790 {
		t.Fatalf("pinned port = %d, want 18790", port)
	}
}

// TestReserveHostPortDoesNotClobberConcurrentSaves confirms the reservation
// is written through a fresh read of the on-disk config, so changes another
// cloister process saved after our stale cfg was loaded survive the pin. It
// also confirms the fresh read makes the allocator respect a pin that only
// exists on disk.
func TestReserveHostPortDoesNotClobberConcurrentSaves(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}

	// The on-disk config carries a concurrent process's pin for "work" that
	// the stale in-memory copy below knows nothing about.
	onDisk := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {Stacks: []string{"agentgrid"}},
		"work": {
			Stacks:            []string{"agentgrid"},
			LocalForwardPorts: map[string]int{"agentgrid": 41200},
		},
	}}
	if err := config.Save(cfgPath, onDisk); err != nil {
		t.Fatalf("seeding on-disk config: %v", err)
	}

	stale := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {Stacks: []string{"agentgrid"}},
		"work":     {Stacks: []string{"agentgrid"}},
	}}

	port, err := reserveHostPort(cfgPath, stale, "innolumi", "agentgrid", 41200)
	if err != nil {
		t.Fatalf("reserveHostPort: %v", err)
	}
	if port == 41200 {
		t.Fatalf("allocator reused work's on-disk pinned port 41200")
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := reloaded.Profiles["work"].LocalForwardPorts["agentgrid"]; got != 41200 {
		t.Fatalf("concurrent pin for work was clobbered: got %d, want 41200", got)
	}
	if got := reloaded.Profiles["innolumi"].LocalForwardPorts["agentgrid"]; got != port {
		t.Fatalf("persisted innolumi pin = %d, want %d", got, port)
	}
	if got := stale.Profiles["innolumi"].LocalForwardPorts["agentgrid"]; got != port {
		t.Fatalf("in-memory mirror = %d, want %d", got, port)
	}
}

// TestReserveHostPortAdoptsOnDiskPin confirms that a pin another process
// persisted after our cfg snapshot is adopted instead of double-allocating.
func TestReserveHostPortAdoptsOnDiskPin(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	onDisk := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {
			Stacks:            []string{"agentgrid"},
			LocalForwardPorts: map[string]int{"agentgrid": 41300},
		},
	}}
	if err := config.Save(cfgPath, onDisk); err != nil {
		t.Fatalf("seeding on-disk config: %v", err)
	}

	stale := &config.Config{Profiles: map[string]*config.Profile{
		"innolumi": {Stacks: []string{"agentgrid"}},
	}}
	port, err := reserveHostPort(cfgPath, stale, "innolumi", "agentgrid", AgentGridHostPort)
	if err != nil {
		t.Fatalf("reserveHostPort: %v", err)
	}
	if port != 41300 {
		t.Fatalf("port = %d, want the on-disk pin 41300", port)
	}
}

// TestReserveHostPortExcludesOtherProfiles confirms first allocation avoids a
// port already pinned by a different profile, the core multi-VM guarantee.
func TestReserveHostPortExcludesOtherProfiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"work": {
			Stacks:            []string{"agentgrid"},
			LocalForwardPorts: map[string]int{"agentgrid": 18765},
		},
		"innolumi": {Stacks: []string{"agentgrid"}},
	}}

	port, err := reserveHostPort(cfgPath, cfg, "innolumi", "agentgrid", 18765)
	if err != nil {
		t.Fatalf("reserveHostPort: %v", err)
	}
	if port == 18765 {
		t.Fatalf("innolumi reused work's pinned port 18765")
	}
}
