// Package tunnel manages SSH reverse port-forwarding tunnels that expose host
// services (clipboard, 1Password agent, audio, etc.) inside cloister VMs.
package tunnel

import (
	"strings"

	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// BuiltinTunnel describes a well-known host service that cloister knows how to
// forward into VMs. Each entry captures the default port the service listens on
// as well as enough metadata to check liveness and guide the user through
// installation.
type BuiltinTunnel struct {
	// Name is the short, human-readable identifier for the service (e.g. "clipboard").
	Name string

	// Port is the TCP port the service listens on on the macOS host (127.0.0.1).
	Port int

	// HealthCheck is either an HTTP URL to GET (service considered available
	// when status 200 is returned) or the literal string "tcp" to perform a
	// raw TCP dial check.
	HealthCheck string

	// Install is the shell command the user should run to install the service
	// on their host machine when it is not already available.
	Install string

	// SetupCmd is an optional cloister sub-command that completes configuration
	// inside the VM after the service is installed (e.g. "cloister setup audio").
	SetupCmd string
}

// Builtins is the canonical set of host services that cloister forwards into
// every running VM. These cover clipboard integration, 1Password SSH/op agent
// forwarding, and PulseAudio for audio passthrough.
var Builtins = []BuiltinTunnel{
	{
		Name:        "clipboard",
		Port:        18339,
		HealthCheck: "http://127.0.0.1:18339/health",
		Install:     "brew install ShunmeiCho/tap/cc-clip",
	},
	{
		Name:        "op-forward",
		Port:        18340,
		HealthCheck: "http://127.0.0.1:18340/health",
		Install:     "brew install ekovshilovsky/tap/op-forward && op-forward service install",
	},
	{
		Name:        "audio",
		Port:        4713,
		HealthCheck: "tcp",
		Install:     "brew install pulseaudio",
		SetupCmd:    "cloister setup audio",
	},
	{
		Name:        "ollama",
		Port:        11434,
		HealthCheck: "tcp",
		Install:     "brew install ollama",
	},
}

// BuiltinTunnelDefs converts the canonical Builtins list into vmconfig.TunnelDef
// entries suitable for inclusion in the VM-side config file. Only the fields
// relevant to the in-VM toolkit (name, port, health endpoint) are carried over.
// The Health field is only set for HTTP endpoints; the literal "tcp" value used
// on the host side is omitted since the VM CLI always performs TCP probes and
// the Health field is reserved for richer HTTP health check URLs.
func BuiltinTunnelDefs() []vmconfig.TunnelDef {
	defs := make([]vmconfig.TunnelDef, len(Builtins))
	for i, b := range Builtins {
		health := b.HealthCheck
		if !strings.HasPrefix(health, "http") {
			health = ""
		}
		defs[i] = vmconfig.TunnelDef{
			Name:   b.Name,
			Port:   b.Port,
			Health: health,
		}
	}
	return defs
}
