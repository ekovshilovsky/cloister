package agent

import (
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Runtime is the interface for agent container lifecycle management. Each
// implementation encapsulates a specific container strategy (e.g., Docker
// containers managed via SSH into a VM, or native macOS processes managed
// via Lume). The vm.Backend parameter is threaded through every method so
// that the Runtime can issue commands against whichever hypervisor backend
// the caller's profile is bound to.
type Runtime interface {
	// Start launches the agent's container(s) for the given profile inside
	// the VM identified by the backend. The implementation is responsible for
	// persisting any container ID or equivalent state needed by subsequent
	// Stop, Status, and Logs calls.
	Start(profile string, cfg *config.AgentConfig, dataDir, workspaceDir string, backend vm.Backend) error

	// Stop tears down the agent's container(s) for the given profile.
	Stop(profile string, backend vm.Backend) error

	// Status inspects the running container and returns its current state.
	Status(profile string, backend vm.Backend) (*AgentStatus, error)

	// Logs streams or tails the agent container's log output.
	Logs(profile string, follow bool, backend vm.Backend) error

	// IsRunning reports whether the agent's primary container is alive.
	IsRunning(profile string, backend vm.Backend) bool
}

// AgentStatus holds the runtime state of an agent, independent of the
// underlying container technology. It is returned by Runtime.Status.
type AgentStatus struct {
	Profile   string
	State     string
	Uptime    string
	Image     string
	Ports     []int
	AutoStart bool
}
