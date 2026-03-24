package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProfileState_RoundTrip verifies that a ProfileState containing Lume-specific
// fields (backend, VM IP, hostname, tunnels, and snapshots) survives a full
// serialise-to-disk and deserialise-from-disk cycle without data loss.
func TestProfileState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	original := &ProfileState{
		Backend: "lume",
		VM: VMState{
			IP:       "192.168.64.10",
			Hostname: "cloister-work.local",
		},
		Tunnels: []TunnelState{
			{Name: "dev", VMPort: 3000, HostPort: 13000, SSHPID: 99999},
		},
		Snapshots: SnapshotState{
			Factory:        "factory-v1",
			User:           "user-checkpoint",
			FactoryCreated: "2025-01-01T00:00:00Z",
			UserCreated:    "2025-06-01T12:00:00Z",
		},
	}

	if err := SaveState(path, original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	if loaded.Backend != original.Backend {
		t.Errorf("Backend: got %q, want %q", loaded.Backend, original.Backend)
	}
	if loaded.VM.IP != original.VM.IP {
		t.Errorf("VM.IP: got %q, want %q", loaded.VM.IP, original.VM.IP)
	}
	if loaded.VM.Hostname != original.VM.Hostname {
		t.Errorf("VM.Hostname: got %q, want %q", loaded.VM.Hostname, original.VM.Hostname)
	}
	if len(loaded.Tunnels) != 1 {
		t.Fatalf("Tunnels length: got %d, want 1", len(loaded.Tunnels))
	}
	if loaded.Tunnels[0].Name != "dev" || loaded.Tunnels[0].VMPort != 3000 ||
		loaded.Tunnels[0].HostPort != 13000 || loaded.Tunnels[0].SSHPID != 99999 {
		t.Errorf("Tunnels[0]: got %+v, want {dev 3000 13000 99999}", loaded.Tunnels[0])
	}
	if loaded.Snapshots.Factory != original.Snapshots.Factory {
		t.Errorf("Snapshots.Factory: got %q, want %q", loaded.Snapshots.Factory, original.Snapshots.Factory)
	}
	if loaded.Snapshots.User != original.Snapshots.User {
		t.Errorf("Snapshots.User: got %q, want %q", loaded.Snapshots.User, original.Snapshots.User)
	}
}

// TestLoadState_Missing confirms that LoadState returns an empty (zero-value)
// ProfileState and no error when the target path does not exist. Callers rely on
// this behaviour to bootstrap fresh profiles without a separate existence check.
func TestLoadState_Missing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState on missing file returned error: %v", err)
	}
	if state == nil {
		t.Fatal("LoadState returned nil state for missing file")
	}

	// A zero ProfileState should have an empty backend and no tunnels.
	if state.Backend != "" {
		t.Errorf("Backend: got %q, want empty string", state.Backend)
	}
	if len(state.Tunnels) != 0 {
		t.Errorf("Tunnels: got %d entries, want 0", len(state.Tunnels))
	}
}

// TestProfileState_CleanStaleTunnels verifies that CleanStaleTunnels removes
// tunnel entries whose SSH process is no longer alive, while retaining entries
// that refer to a live PID (the current process itself, which is guaranteed
// to be running during the test).
func TestProfileState_CleanStaleTunnels(t *testing.T) {
	livePID := os.Getpid()

	state := &ProfileState{
		Tunnels: []TunnelState{
			// A PID that is almost certainly dead: PID 1 on macOS is launchd
			// and is not an SSH process. Using a large, unlikely PID is fragile,
			// so instead use PID 0 which is never a valid user-space process.
			{Name: "stale", VMPort: 8080, HostPort: 18080, SSHPID: 0},
			// The test process itself is alive, so this entry must be retained.
			{Name: "live", VMPort: 9090, HostPort: 19090, SSHPID: livePID},
		},
	}

	state.CleanStaleTunnels()

	if len(state.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel after cleaning, got %d: %+v", len(state.Tunnels), state.Tunnels)
	}
	if state.Tunnels[0].Name != "live" {
		t.Errorf("expected retained tunnel to be %q, got %q", "live", state.Tunnels[0].Name)
	}
}

// TestProfileState_FindTunnel verifies that FindTunnel returns the correct
// TunnelState when a matching VM port is present, and nil when no match exists.
func TestProfileState_FindTunnel(t *testing.T) {
	state := &ProfileState{
		Tunnels: []TunnelState{
			{Name: "web", VMPort: 3000, HostPort: 13000, SSHPID: 1234},
			{Name: "api", VMPort: 8080, HostPort: 18080, SSHPID: 1235},
		},
	}

	found := state.FindTunnel(3000)
	if found == nil {
		t.Fatal("FindTunnel(3000): expected non-nil result")
	}
	if found.Name != "web" {
		t.Errorf("FindTunnel(3000): got name %q, want %q", found.Name, "web")
	}

	missing := state.FindTunnel(5000)
	if missing != nil {
		t.Errorf("FindTunnel(5000): expected nil, got %+v", missing)
	}
}

// TestProfileState_HostPortInUse confirms that HostPortInUse returns true when
// any tunnel entry occupies the queried host port, and false otherwise.
func TestProfileState_HostPortInUse(t *testing.T) {
	state := &ProfileState{
		Tunnels: []TunnelState{
			{Name: "web", VMPort: 3000, HostPort: 13000, SSHPID: 1234},
		},
	}

	if !state.HostPortInUse(13000) {
		t.Error("HostPortInUse(13000): expected true, got false")
	}
	if state.HostPortInUse(14000) {
		t.Error("HostPortInUse(14000): expected false, got true")
	}
}
