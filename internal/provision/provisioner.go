// Package provision defines the Provisioner interface and provides
// backward-compatible wrapper functions that delegate to the default Linux
// provisioner with the Colima backend. New callers should import the concrete
// provisioner package (e.g. linux) and supply an explicit vm.Backend.
package provision

import (
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Provisioner abstracts the platform-specific provisioning sequence that
// installs base tools, toolchain stacks, and configuration templates inside a
// VM. Each implementation targets a specific guest OS (e.g. Linux, macOS).
type Provisioner interface {
	// Run executes the full provisioning sequence for the given profile.
	Run(profile string, p *config.Profile, backend vm.Backend) error

	// DeployConfig re-deploys runtime configuration files (bashrc, VM config)
	// into an already-provisioned VM so that configuration changes take effect
	// without a full rebuild.
	DeployConfig(profile string, p *config.Profile, backend vm.Backend) error
}
