package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"gopkg.in/yaml.v3"
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

// TestProfileWithPolicies verifies that tunnel_policy and mount_policy fields
// unmarshal correctly from YAML into the Profile struct, covering both the
// unset (omitted), explicit list, and scalar "none" forms.
func TestProfileWithPolicies(t *testing.T) {
	yamlInput := `
profiles:
  work:
    memory: 8
    stacks: [web, ollama]
  agent:
    headless: true
    tunnel_policy: [ollama]
    mount_policy: none
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(yamlInput), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	work := cfg.Profiles["work"]
	if work.TunnelPolicy.IsSet {
		t.Error("work tunnel policy should be unset")
	}

	agent := cfg.Profiles["agent"]
	if !agent.TunnelPolicy.IsSet {
		t.Fatal("agent tunnel policy should be set")
	}
	if len(agent.TunnelPolicy.Names) != 1 || agent.TunnelPolicy.Names[0] != "ollama" {
		t.Errorf("agent tunnel policy names = %v, want [ollama]", agent.TunnelPolicy.Names)
	}
	if !agent.MountPolicy.IsSet || agent.MountPolicy.Mode != "none" {
		t.Errorf("agent mount policy = %+v, want mode=none", agent.MountPolicy)
	}
}

// TestDefaultStartDirLowercase verifies that DefaultStartDir uses lowercase
// ~/code rather than the legacy capitalised ~/Code form.
func TestDefaultStartDirLowercase(t *testing.T) {
	if config.DefaultStartDir != "~/code" {
		t.Errorf("DefaultStartDir = %q, want \"~/code\"", config.DefaultStartDir)
	}
}

// TestAgentConfigRoundTrip verifies that an agent profile with all AgentConfig
// fields set round-trips correctly through YAML unmarshal.
func TestAgentConfigRoundTrip(t *testing.T) {
	input := `
profiles:
  openclaw:
    headless: true
    memory: 4
    stacks: [web]
    agent:
      type: openclaw
      image: openclaw/openclaw:latest
      ports: [3000]
      auto_start: true
      env:
        ANTHROPIC_API_KEY: test-key
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Profiles["openclaw"]
	if p.Agent == nil {
		t.Fatal("agent config should not be nil")
	}
	if p.Agent.Type != "openclaw" {
		t.Errorf("Type = %q, want openclaw", p.Agent.Type)
	}
	if p.Agent.Image != "openclaw/openclaw:latest" {
		t.Errorf("Image = %q, want openclaw/openclaw:latest", p.Agent.Image)
	}
	if len(p.Agent.Ports) != 1 || p.Agent.Ports[0] != 3000 {
		t.Errorf("Ports = %v, want [3000]", p.Agent.Ports)
	}
	if !p.Agent.AutoStart {
		t.Error("AutoStart should be true")
	}
	if p.Agent.Env["ANTHROPIC_API_KEY"] != "test-key" {
		t.Errorf("Env = %v, want ANTHROPIC_API_KEY=test-key", p.Agent.Env)
	}
}

// TestProfileWithoutAgent verifies that profiles without an agent block
// have a nil Agent field, distinguishing them from agent profiles.
func TestProfileWithoutAgent(t *testing.T) {
	input := `
profiles:
  work:
    memory: 4
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Profiles["work"]
	if p.Agent != nil {
		t.Error("agent config should be nil for non-agent profile")
	}
}

func TestLoadSetsBackendDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `profiles:
  work:
    memory: 8
  agent:
    memory: 4
    backend: lume
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Profiles["work"].Backend != "colima" {
		t.Errorf("work.Backend = %q, want %q", cfg.Profiles["work"].Backend, "colima")
	}
	if cfg.Profiles["agent"].Backend != "lume" {
		t.Errorf("agent.Backend = %q, want %q", cfg.Profiles["agent"].Backend, "lume")
	}
}

func TestLoadSetsConfigVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `profiles:
  work:
    memory: 8
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != 2 {
		t.Errorf("Version = %d, want 2", cfg.Version)
	}
}

func TestSaveRotatesPrev(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := &config.Config{
		Version:  2,
		Profiles: map[string]*config.Profile{"old": {Memory: 4, Backend: "colima"}},
	}
	if err := config.Save(path, original); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	updated := &config.Config{
		Version:  2,
		Profiles: map[string]*config.Profile{"new": {Memory: 8, Backend: "lume"}},
	}
	if err := config.Save(path, updated); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	prevPath := path + ".prev"
	prev, err := config.Load(prevPath)
	if err != nil {
		t.Fatalf("Load .prev: %v", err)
	}
	if _, ok := prev.Profiles["old"]; !ok {
		t.Error("expected .prev to contain profile 'old'")
	}
	current, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load current: %v", err)
	}
	if _, ok := current.Profiles["new"]; !ok {
		t.Error("expected current config to contain profile 'new'")
	}
}

func TestBackendRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := &config.Config{
		Version: 2,
		Profiles: map[string]*config.Profile{
			"dev": {Memory: 8, Backend: "colima"},
			"oc":  {Memory: 4, Backend: "lume"},
		},
	}
	if err := config.Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 2 {
		t.Errorf("Version = %d, want 2", loaded.Version)
	}
	if loaded.Profiles["dev"].Backend != "colima" {
		t.Errorf("dev.Backend = %q, want colima", loaded.Profiles["dev"].Backend)
	}
	if loaded.Profiles["oc"].Backend != "lume" {
		t.Errorf("oc.Backend = %q, want lume", loaded.Profiles["oc"].Backend)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "backend:") {
		t.Error("expected raw YAML to contain 'backend:' field")
	}
}
