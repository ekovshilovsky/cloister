// Package config provides types and I/O helpers for the cloister configuration
// file located at ~/.cloister/config.yaml.
package config

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level structure persisted to disk. All fields are optional
// so that a minimal or empty file remains valid.
type Config struct {
	// MemoryBudget is the total gigabytes available for VM allocation across all
	// active profiles. When zero the budget is determined at runtime via
	// CalculateBudget.
	MemoryBudget int `yaml:"memory_budget,omitempty"`

	// Profiles is the named set of VM profiles defined by the user.
	Profiles map[string]*Profile `yaml:"profiles"`

	// Tunnels lists persistent port-forwarding rules that are applied whenever
	// any VM in the profile is running.
	Tunnels []TunnelConfig `yaml:"tunnels,omitempty"`
}

// Profile describes the resource and environment configuration for a single
// named VM. Zero values indicate "use the default".
type Profile struct {
	// Memory is the VM memory allocation in gigabytes.
	Memory int `yaml:"memory,omitempty"`

	// StartDir is the directory opened in the terminal when attaching to the VM.
	StartDir string `yaml:"start_dir,omitempty"`

	// Color is the accent color used for this profile in terminal output.
	Color string `yaml:"color,omitempty"`

	// Stacks lists the toolchain stacks to provision inside the VM (e.g. "node", "python").
	Stacks []string `yaml:"stacks,omitempty"`

	// GPGSigning enables automatic GPG commit-signing configuration inside the VM.
	GPGSigning bool `yaml:"gpg_signing,omitempty"`

	// Disk is the VM disk size in gigabytes.
	Disk int `yaml:"disk,omitempty"`

	// CPU is the number of virtual CPUs assigned to the VM.
	CPU int `yaml:"cpu,omitempty"`

	// DotnetVersion pins the .NET SDK version installed in the VM.
	DotnetVersion string `yaml:"dotnet_version,omitempty"`

	// NodeVersion pins the Node.js version installed in the VM.
	NodeVersion string `yaml:"node_version,omitempty"`

	// PythonVersion pins the Python version installed in the VM.
	PythonVersion string `yaml:"python_version,omitempty"`

	// GoVersion pins the Go toolchain version installed in the VM.
	GoVersion string `yaml:"go_version,omitempty"`

	// RustVersion pins the Rust toolchain version installed in the VM.
	RustVersion string `yaml:"rust_version,omitempty"`

	// TerraformVersion pins the Terraform CLI version installed in the VM.
	TerraformVersion string `yaml:"terraform_version,omitempty"`

	// Headless suppresses attaching a terminal window when the VM starts.
	Headless bool `yaml:"headless,omitempty"`

	// TunnelPolicy controls which host services are forwarded into the VM.
	// When omitted, interactive profiles default to "auto" and headless to "none."
	TunnelPolicy ResourcePolicy `yaml:"tunnel_policy,omitempty"`

	// MountPolicy controls which host directories are mounted into the VM.
	// When omitted, interactive profiles default to "auto" and headless to "none."
	MountPolicy ResourcePolicy `yaml:"mount_policy,omitempty"`

	// ClaudeLocal enables offline Claude Code by pointing it at the host's
	// Ollama server via the Anthropic Messages API compatibility layer.
	// Requires the ollama stack and a running Ollama instance on the host.
	ClaudeLocal bool `yaml:"claude_local,omitempty"`
}

// TunnelConfig describes a single persistent port-forwarding rule between the
// host and a running VM.
type TunnelConfig struct {
	// Name is a human-readable label used to reference this tunnel.
	Name string `yaml:"name"`

	// HostPort is the port exposed on the macOS host.
	HostPort int `yaml:"host_port"`

	// VMPort is the port inside the VM. When zero it defaults to HostPort.
	VMPort int `yaml:"vm_port,omitempty"`

	// HealthCheck is an optional URL polled to determine whether the service
	// behind the tunnel is ready.
	HealthCheck string `yaml:"health_check,omitempty"`
}

// ConfigDir returns the path to the cloister configuration directory,
// i.e. ~/.cloister.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloister"), nil
}

// ConfigPath returns the canonical path to the configuration file,
// i.e. ~/.cloister/config.yaml.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads and deserialises the YAML configuration at path. If the file does
// not exist a zero-valued Config is returned with the Profiles map initialised
// so callers can safely insert entries without a nil-map panic.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Profiles: make(map[string]*Profile),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Ensure the Profiles map is non-nil even when the YAML file omits the key.
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]*Profile)
	}

	return cfg, nil
}

// Save serialises cfg to YAML and writes it to path, creating any missing
// parent directories with mode 0700.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}
