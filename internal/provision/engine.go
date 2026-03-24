package provision

import (
	"embed"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/provision/linux"
	"github.com/ekovshilovsky/cloister/internal/vm/colima"
	"github.com/ekovshilovsky/cloister/internal/vmconfig"
)

// defaultEngine is the Linux provisioner used by the backward-compatible
// package-level functions. Callers that need to select a different provisioner
// or backend should use the linux.Engine (or another Provisioner) directly.
var defaultEngine = &linux.Engine{}

// defaultBackend is the Colima backend used by the backward-compatible
// package-level functions. Callers that need a different backend should pass
// it explicitly to the provisioner.
var defaultBackend = &colima.Backend{}

// Scripts re-exports the embedded script filesystem from the Linux provisioner
// so that existing callers (e.g. cmd/addstack.go) continue to compile without
// import path changes.
var Scripts embed.FS = linux.Scripts

// Templates re-exports the embedded template filesystem from the Linux
// provisioner for backward compatibility.
var Templates embed.FS = linux.Templates

// Run delegates to the Linux provisioner with the Colima backend.
// Deprecated: callers should use linux.Engine directly with their resolved backend.
func Run(profile string, p *config.Profile) error {
	return defaultEngine.Run(profile, p, defaultBackend)
}

// DeployBashrc re-renders and deploys the managed bashrc into a running VM
// using the default Linux provisioner and Colima backend.
// Deprecated: callers should use linux.Engine.DeployBashrc directly.
func DeployBashrc(profile string, p *config.Profile) error {
	return defaultEngine.DeployBashrc(profile, p, defaultBackend)
}

// DeployVMConfig writes the cloister-vm config file into the VM using the
// default Linux provisioner and Colima backend.
// Deprecated: callers should use linux.Engine.DeployVMConfig directly.
func DeployVMConfig(profile string, p *config.Profile, tunnelDefs []vmconfig.TunnelDef, workspaceDir string) error {
	return defaultEngine.DeployVMConfig(profile, p, defaultBackend, tunnelDefs, workspaceDir)
}

// ResolveStartDir re-exports from the linux package for backward compatibility.
func ResolveStartDir(startDir string) string {
	return linux.ResolveStartDir(startDir)
}
