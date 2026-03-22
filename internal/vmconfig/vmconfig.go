package vmconfig

// Config is the schema for ~/.cloister-vm/config.json, written by the host
// provisioning engine and read by the cloister-vm CLI inside the VM. Both
// sides import this package to avoid struct duplication.
type Config struct {
	// Profile is the cloister profile name this VM belongs to.
	Profile string `json:"profile"`

	// Tunnels lists the host services that may be tunneled into this VM.
	// Availability is checked at runtime via TCP probe.
	Tunnels []TunnelDef `json:"tunnels"`

	// Workspace is the absolute host path mounted as the VM's workspace.
	Workspace string `json:"workspace"`

	// ClaudeLocal indicates whether Claude Code should use the local Ollama
	// server via Anthropic API compatibility instead of Anthropic's cloud.
	ClaudeLocal bool `json:"claude_local"`
}

// TunnelDef describes a single host service tunnel that may be forwarded
// into the VM via SSH reverse port forwarding.
type TunnelDef struct {
	// Name is the human-readable identifier (e.g., "clipboard", "ollama").
	Name string `json:"name"`

	// Port is the TCP port the tunnel listens on inside the VM.
	Port int `json:"port"`

	// Health is an optional HTTP URL for richer health checking beyond TCP.
	Health string `json:"health,omitempty"`
}
