// Package config provides types and I/O helpers for the cloister configuration
// file located at ~/.cloister/config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CurrentVersion is the newest configuration schema understood by Cloister.
const CurrentVersion = 3

const (
	// WorkspaceModeVirtiofs preserves the legacy direct host mount behavior.
	WorkspaceModeVirtiofs = "virtiofs"

	// WorkspaceModeBroker selects a project-scoped synchronized guest copy.
	WorkspaceModeBroker = "broker"
)

// Config is the top-level structure persisted to disk. All fields are optional
// so that a minimal or empty file remains valid.
type Config struct {
	// Version is the schema version of this config file. Version 3 adds the
	// opt-in workspace broker; used to detect and apply schema
	// migrations on load. Not omitempty so it is always serialized to disk.
	Version int `yaml:"version"`

	// MemoryBudget is the total gigabytes available for VM allocation across all
	// active profiles. When zero the budget is determined at runtime via
	// CalculateBudget.
	MemoryBudget int `yaml:"memory_budget,omitempty"`

	// Profiles is the named set of VM profiles defined by the user.
	Profiles map[string]*Profile `yaml:"profiles"`

	// Tunnels lists persistent port-forwarding rules that are applied whenever
	// any VM in the profile is running.
	Tunnels []TunnelConfig `yaml:"tunnels,omitempty"`

	// Ollama holds host-side Ollama settings for VM-bridged access.
	Ollama *OllamaConfig `yaml:"ollama,omitempty"`
}

// Profile describes the resource and environment configuration for a single
// named VM. Zero values indicate "use the default".
type Profile struct {
	// Backend selects the VM hypervisor ("colima" or "lume"). When empty,
	// defaults to "colima" for backward compatibility. Set automatically
	// by --openclaw (lume) or overridden with --backend on create.
	Backend string `yaml:"backend"`

	// Memory is the VM memory allocation in gigabytes.
	Memory int `yaml:"memory,omitempty"`

	// StartDir is the directory opened in the terminal when attaching to the VM.
	StartDir string `yaml:"start_dir,omitempty"`

	// Workspace selects how project files reach the VM. Existing and omitted
	// values resolve to virtiofs. Broker mode is an explicit opt-in because it
	// provides a synchronized copy rather than local-filesystem equivalence.
	Workspace WorkspaceConfig `yaml:"workspace,omitempty"`

	// Color is the accent color used for this profile in terminal output.
	Color string `yaml:"color,omitempty"`

	// Stacks lists the toolchain stacks to provision inside the VM (e.g. "node", "python").
	Stacks []string `yaml:"stacks,omitempty"`

	// GPGSigning enables automatic GPG commit-signing configuration inside the
	// VM by forwarding the macOS host's gpg-agent extra-socket over SSH. The
	// VM holds only public keys; signing executes on the host. Mutually
	// exclusive with GpgLocal.
	GPGSigning bool `yaml:"gpg_signing,omitempty"`

	// GpgLocal opts the VM out of cloister's host-agent forwarding and leaves
	// the local gpg-agent enabled so the user can manage GPG keys inside the
	// VM directly. By default cloister masks the local gpg-agent on every VM
	// (so the runtime socket path is free for the cloister-managed reverse
	// tunnel that GPGSigning attaches); setting GpgLocal: true skips that
	// mask, allowing `gpg --gen-key`, key import, etc. inside the VM to work
	// against a normal local agent. Mutually exclusive with GPGSigning.
	GpgLocal bool `yaml:"gpg_local,omitempty"`

	// MountInotify enables Colima's host→VM inotify event propagation for
	// every mount. Colima's own default is true; cloister flips the default
	// to false because the implementation accumulates one open directory
	// handle per watched subdirectory and never aggressively reclaims them.
	// Profiles that mount large directory trees (~/.claude/skills, ~/.agents,
	// ~/.ollama/models) accrue ~1,500 fds/hour per active VM, exhausting
	// kern.maxfiles within a week and triggering "FS pagein error: 23 Too
	// many open files in system" crashes for sandboxed apps on the host.
	// Set MountInotify: true on profiles that genuinely need host→VM
	// filesystem-event forwarding (e.g., when running an in-VM dev server
	// like webpack or vite that watches host-mounted source for hot reload).
	MountInotify bool `yaml:"mount_inotify,omitempty"`

	// Disk is the VM data disk size in gigabytes. Maps to Colima's `--disk`
	// flag which controls the secondary disk used for container storage
	// (Docker images, container volumes, /var/lib/docker, etc.).
	Disk int `yaml:"disk,omitempty"`

	// RootDisk is the VM root filesystem disk size in gigabytes. Maps to
	// Colima's `--root-disk` flag which controls the OS disk where language
	// SDKs (.NET, pyenv-built Python, Go toolchains) are installed
	// system-wide. Colima's default is 20 GiB which is too small for
	// stack-heavy profiles. When zero, cmd/create.go applies the package
	// default (DefaultRootDiskGB). Backend.Start passes this through to
	// `--root-disk` only when non-zero so existing VMs with root disks
	// already created at the prior default size keep working.
	RootDisk int `yaml:"root_disk,omitempty"`

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

	// Agent holds the Docker container configuration for headless agent
	// profiles. Nil for interactive profiles.
	Agent *AgentConfig `yaml:"agent,omitempty"`

	// LocalForwardPorts pins the macOS-side port for each stack-owned local
	// forward (VM listener published on the host), keyed by service name
	// (e.g. "agentgrid"). Unlike reverse tunnels, local forwards bind a host
	// port, so two running VMs that expose the same service would otherwise
	// fight over one port. The first successful forward records the chosen
	// port here so that later restarts reuse exactly that endpoint and clients
	// paired against it keep working. A reserved port is never silently
	// changed: if it is occupied on a later start, cloister fails and reports
	// how to recover.
	LocalForwardPorts map[string]int `yaml:"local_forward_ports,omitempty"`
}

// WorkspaceConfig controls project transport for one profile.
type WorkspaceConfig struct {
	// Mode is "virtiofs" or "broker". The empty value migrates in memory to
	// virtiofs so existing profiles retain their current behavior.
	Mode string `yaml:"mode,omitempty"`

	// Ignore adds project-relative Git-style exclusions before Cloister's
	// mandatory, non-negatable broker exclusions.
	Ignore []string `yaml:"ignore,omitempty"`
}

// AgentConfig describes the Docker container configuration for a headless
// agent running inside the VM. When nil, the profile is not an agent profile.
type AgentConfig struct {
	// Type identifies the agent runtime (e.g., "openclaw"). Used to apply
	// runtime-specific defaults for image, ports, and Docker flags.
	Type string `yaml:"type"`

	// Image is the Docker image to run inside the VM.
	Image string `yaml:"image"`

	// Ports lists the TCP ports published from the container to the VM's
	// localhost. These are NOT exposed to the host without an SSH tunnel.
	Ports []int `yaml:"ports,omitempty"`

	// AutoStart controls whether the agent container is started automatically
	// when the VM boots. Set to true by `agent start`, false by `agent stop`.
	AutoStart bool `yaml:"auto_start,omitempty"`

	// Env holds optional environment variable overrides injected into the
	// Docker container at startup. Used as a fallback when op-forward is
	// not available for credential injection.
	Env map[string]string `yaml:"env,omitempty"`
}

// HasStack reports whether the named stack is present in the profile's stack list.
func (p *Profile) HasStack(name string) bool {
	for _, s := range p.Stacks {
		if s == name {
			return true
		}
	}
	return false
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

// OllamaConfig holds host-side Ollama settings for VM-bridged operation.
type OllamaConfig struct {
	// Host is the address Ollama binds to, reachable from the VM.
	// Empty means auto-detect the VM bridge gateway IP.
	Host string `yaml:"host,omitempty"`

	// Tuning holds performance tuning env vars applied to Ollama's LaunchAgent.
	Tuning OllamaTuning `yaml:"tuning,omitempty"`
}

// OllamaTuning holds Ollama environment variable overrides.
type OllamaTuning struct {
	FlashAttention  *bool  `yaml:"flash_attention,omitempty"`
	KVCacheType     string `yaml:"kv_cache_type,omitempty"`
	MaxLoadedModels *int   `yaml:"max_loaded_models,omitempty"`
	NumParallel     *int   `yaml:"num_parallel,omitempty"`
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

	// Migrate legacy zero values in memory. Loading never writes the file, so
	// schema version 3 is persisted only by a later authorized Save operation.
	for name, p := range cfg.Profiles {
		if p == nil {
			return nil, fmt.Errorf("profile %q is null", name)
		}
		if p.Backend == "" {
			p.Backend = "colima"
		}
		if p.Workspace.Mode == "" {
			p.Workspace.Mode = WorkspaceModeVirtiofs
		}
		if p.Workspace.Mode != WorkspaceModeVirtiofs && p.Workspace.Mode != WorkspaceModeBroker {
			return nil, fmt.Errorf("profile %q has unsupported workspace mode %q; use %q or %q", name, p.Workspace.Mode, WorkspaceModeVirtiofs, WorkspaceModeBroker)
		}
	}

	// Validate: gpg_signing (host-agent forwarding) and gpg_local (in-VM
	// agent management) are mutually exclusive — they would compete for the
	// same runtime socket path inside the VM. Reject loudly at load time so
	// the user fixes the YAML rather than discovering the conflict during
	// signing.
	for name, p := range cfg.Profiles {
		if p.GPGSigning && p.GpgLocal {
			return nil, fmt.Errorf("profile %q sets both gpg_signing and gpg_local; pick one (gpg_signing forwards the host's gpg-agent; gpg_local keeps the VM's local agent enabled for in-VM key management)", name)
		}
	}
	if cfg.Version < CurrentVersion {
		cfg.Version = CurrentVersion
	}

	return cfg, nil
}

// Save serialises cfg to YAML and writes it to path, creating any missing
// parent directories with mode 0700.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	persisted := *cfg
	persisted.Version = CurrentVersion
	data, err := yaml.Marshal(&persisted)
	if err != nil {
		return err
	}

	// Rotate existing config to .prev before writing. The rename syscall is
	// atomic on the same filesystem, so a crash between the rename and the
	// subsequent write leaves .prev intact as the most recent known-good
	// state. The rename and write are not atomic as a pair.
	if _, statErr := os.Stat(path); statErr == nil {
		if renameErr := os.Rename(path, path+".prev"); renameErr != nil {
			return fmt.Errorf("rotating config to .prev: %w", renameErr)
		}
	}

	return os.WriteFile(path, data, 0o600)
}
