// Package tunnel manages SSH reverse port-forwarding tunnels that expose host
// services (clipboard, 1Password agent, audio, etc.) inside cloister VMs.
package tunnel

import (
	"fmt"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/gpgforward"
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
	// Zero indicates a Unix-socket tunnel (see HostSocketResolver).
	Port int

	// HealthCheck is one of:
	//   - "tcp"             raw TCP dial against 127.0.0.1:Port
	//   - an HTTP URL       service considered available when GET returns 200
	//   - "socket"          host-side Unix socket exists and is a socket file
	HealthCheck string

	// HostSocketResolver returns the absolute path to the host-side Unix socket
	// for socket tunnels. It is invoked at discovery time so callers can defer
	// lookups that depend on host state (e.g. paths persisted by a separate
	// preflight command). Nil for TCP/HTTP tunnels.
	HostSocketResolver func() (string, error)

	// GuestSocket is the absolute path inside the VM where the forwarded socket
	// should appear. Two placeholders are resolved at tunnel-start time so the
	// field can stay user-agnostic:
	//   "$HOME" → the VM user's home directory (via `echo $HOME` over SSH)
	//   "$UID"  → the VM user's numeric UID    (via `id -u` over SSH)
	// Empty for TCP/HTTP tunnels.
	GuestSocket string

	// RequiresFlag names a boolean feature flag on the profile that must be set
	// for this builtin to be considered. An empty string means always-on.
	// Currently recognized values: "GPGSigning".
	RequiresFlag string

	// Install is the shell command the user should run to install the service
	// on their host machine when it is not already available.
	Install string

	// SetupCmd is an optional cloister sub-command that completes configuration
	// inside the VM after the service is installed (e.g. "cloister setup audio").
	SetupCmd string
}

// Builtins is the canonical set of host services that cloister forwards into
// every running VM. These cover clipboard integration, 1Password SSH/op agent
// forwarding, PulseAudio for audio passthrough, and gpg-agent forwarding for
// GPG-signed commits.
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
		Name:               "gpg-forward",
		HealthCheck:        "socket",
		HostSocketResolver: resolveGPGForwardHostSocket,
		// /run/user/<uid>/gnupg/ is GnuPG 2.4's default socket directory on
		// systemd-managed Linux (overrides the legacy ~/.gnupg/ location).
		// gpg in the VM looks here, so the forwarded socket has to land here
		// too — putting it at $HOME/.gnupg/S.gpg-agent silently does nothing.
		GuestSocket:  "/run/user/$UID/gnupg/S.gpg-agent",
		RequiresFlag: "GPGSigning",
		Install:      "cloister setup gpg-forward",
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

// resolveGPGForwardHostSocket returns the host-side gpg-agent extra-socket
// path that `cloister setup gpg-forward` persisted to the cloister state
// directory. An empty result is treated by callers as "preflight not run yet"
// and surfaces a clear install hint rather than silently skipping the tunnel.
func resolveGPGForwardHostSocket() (string, error) {
	p, err := gpgforward.LoadHostSocketPath()
	if err != nil {
		return "", err
	}
	if p == "" {
		return "", fmt.Errorf("host preflight not run (cloister setup gpg-forward)")
	}
	return p, nil
}

// BuiltinTunnelDefs converts the canonical Builtins list into vmconfig.TunnelDef
// entries suitable for inclusion in the VM-side config file. The Health field
// carries through only for HTTP endpoints; the literal "tcp" / "socket" values
// used on the host side are omitted since the VM CLI handles probe-type
// selection from the Port/Socket fields directly. Socket builtins (Port == 0)
// are emitted with their GuestSocket path in the Socket field so the in-VM
// toolkit can stat the socket for connectivity instead of TCP-probing port 0.
func BuiltinTunnelDefs() []vmconfig.TunnelDef {
	defs := make([]vmconfig.TunnelDef, 0, len(Builtins))
	for _, b := range Builtins {
		health := b.HealthCheck
		if !strings.HasPrefix(health, "http") {
			health = ""
		}
		defs = append(defs, vmconfig.TunnelDef{
			Name:   b.Name,
			Port:   b.Port,
			Health: health,
			Socket: b.GuestSocket,
		})
	}
	return defs
}
