package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// TestLoadNoFile verifies that Load returns an empty Config with a non-nil
// Profiles map when the target file does not exist.
func TestLoadNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error for missing file: %v", err)
	}
	if cfg.Profiles == nil {
		t.Error("Profiles map must be non-nil even when file does not exist")
	}
	if len(cfg.Profiles) != 0 {
		t.Errorf("expected empty Profiles map, got %d entries", len(cfg.Profiles))
	}
}

// TestLoadValidYAML verifies that Load correctly parses a well-formed YAML file.
func TestLoadValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `memory_budget: 16
profiles:
  work:
    memory: 8
    start_dir: ~/Work
    color: blue
    cpu: 2
    disk: 60
tunnels:
  - name: web
    host_port: 8080
    vm_port: 80
    health_check: http://localhost:80/health
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("failed to write fixture file: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}

	if cfg.MemoryBudget != 16 {
		t.Errorf("MemoryBudget: got %d, want 16", cfg.MemoryBudget)
	}

	p, ok := cfg.Profiles["work"]
	if !ok {
		t.Fatal("expected profile 'work' to be present")
	}
	if p.Memory != 8 {
		t.Errorf("Profile.Memory: got %d, want 8", p.Memory)
	}
	if p.StartDir != "~/Work" {
		t.Errorf("Profile.StartDir: got %q, want %q", p.StartDir, "~/Work")
	}
	if p.Color != "blue" {
		t.Errorf("Profile.Color: got %q, want %q", p.Color, "blue")
	}
	if p.CPU != 2 {
		t.Errorf("Profile.CPU: got %d, want 2", p.CPU)
	}
	if p.Disk != 60 {
		t.Errorf("Profile.Disk: got %d, want 60", p.Disk)
	}

	if len(cfg.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(cfg.Tunnels))
	}
	tun := cfg.Tunnels[0]
	if tun.Name != "web" {
		t.Errorf("Tunnel.Name: got %q, want %q", tun.Name, "web")
	}
	if tun.HostPort != 8080 {
		t.Errorf("Tunnel.HostPort: got %d, want 8080", tun.HostPort)
	}
	if tun.VMPort != 80 {
		t.Errorf("Tunnel.VMPort: got %d, want 80", tun.VMPort)
	}
}

// TestSaveAndLoadRoundtrip verifies that a Config written by Save can be
// correctly recovered by Load with all values intact.
func TestSaveAndLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "config.yaml")

	original := &config.Config{
		MemoryBudget: 22,
		Profiles: map[string]*config.Profile{
			"dev": {
				Memory:        8,
				StartDir:      "~/Projects",
				Color:         "green",
				CPU:           4,
				Disk:          80,
				GPGSigning:    true,
				NodeVersion:   "20",
				PythonVersion: "3.12",
			},
		},
		Tunnels: []config.TunnelConfig{
			{
				Name:        "api",
				HostPort:    3000,
				VMPort:      3000,
				HealthCheck: "http://localhost:3000/ping",
			},
		},
	}

	if err := config.Save(path, original); err != nil {
		t.Fatalf("Save returned unexpected error: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after Save returned unexpected error: %v", err)
	}

	if loaded.MemoryBudget != original.MemoryBudget {
		t.Errorf("MemoryBudget: got %d, want %d", loaded.MemoryBudget, original.MemoryBudget)
	}

	dev, ok := loaded.Profiles["dev"]
	if !ok {
		t.Fatal("expected profile 'dev' to be present after roundtrip")
	}
	orig := original.Profiles["dev"]
	if dev.Memory != orig.Memory {
		t.Errorf("Profile.Memory: got %d, want %d", dev.Memory, orig.Memory)
	}
	if dev.GPGSigning != orig.GPGSigning {
		t.Errorf("Profile.GPGSigning: got %v, want %v", dev.GPGSigning, orig.GPGSigning)
	}
	if dev.NodeVersion != orig.NodeVersion {
		t.Errorf("Profile.NodeVersion: got %q, want %q", dev.NodeVersion, orig.NodeVersion)
	}

	if len(loaded.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel after roundtrip, got %d", len(loaded.Tunnels))
	}
	if loaded.Tunnels[0].Name != "api" {
		t.Errorf("Tunnel.Name: got %q, want %q", loaded.Tunnels[0].Name, "api")
	}
}

// TestCalculateBudget verifies the budget calculation logic at representative
// values, including the minimum floor.
func TestCalculateBudget(t *testing.T) {
	cases := []struct {
		totalRAMGB int
		want       int
	}{
		{totalRAMGB: 32, want: 22},
		{totalRAMGB: 12, want: 4}, // minimum floor applies (12-10=2 < 4)
		{totalRAMGB: 14, want: 4}, // 14-10=4, exactly at the floor
		{totalRAMGB: 20, want: 10},
	}

	for _, tc := range cases {
		got := config.CalculateBudget(tc.totalRAMGB)
		if got != tc.want {
			t.Errorf("CalculateBudget(%d) = %d, want %d", tc.totalRAMGB, got, tc.want)
		}
	}
}

// TestApplyDefaults verifies that Profile.ApplyDefaults fills in zero-value
// fields with the expected default constants.
func TestApplyDefaults(t *testing.T) {
	p := &config.Profile{}
	p.ApplyDefaults()

	if p.Memory != config.DefaultMemory {
		t.Errorf("Memory: got %d, want %d", p.Memory, config.DefaultMemory)
	}
	if p.Disk != config.DefaultDisk {
		t.Errorf("Disk: got %d, want %d", p.Disk, config.DefaultDisk)
	}
	if p.CPU != config.DefaultCPU {
		t.Errorf("CPU: got %d, want %d", p.CPU, config.DefaultCPU)
	}
	if p.StartDir != config.DefaultStartDir {
		t.Errorf("StartDir: got %q, want %q", p.StartDir, config.DefaultStartDir)
	}
}

// TestApplyDefaultsDoesNotOverwrite verifies that ApplyDefaults does not
// overwrite fields that have already been set by the user.
func TestApplyDefaultsDoesNotOverwrite(t *testing.T) {
	p := &config.Profile{
		Memory:   16,
		Disk:     100,
		CPU:      8,
		StartDir: "~/Custom",
	}
	p.ApplyDefaults()

	if p.Memory != 16 {
		t.Errorf("Memory should not be overwritten: got %d", p.Memory)
	}
	if p.Disk != 100 {
		t.Errorf("Disk should not be overwritten: got %d", p.Disk)
	}
	if p.CPU != 8 {
		t.Errorf("CPU should not be overwritten: got %d", p.CPU)
	}
	if p.StartDir != "~/Custom" {
		t.Errorf("StartDir should not be overwritten: got %q", p.StartDir)
	}
}
