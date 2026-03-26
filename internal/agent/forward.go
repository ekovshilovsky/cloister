package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// buildForwardSSHArgs constructs the SSH arguments for a local port forward.
// The resulting command runs ssh in the background (-fN) and binds the given
// port on the specified bind address to the same port on the VM's loopback via -L.
//
// When bindAll is true the forward binds to 0.0.0.0 (all interfaces), exposing
// the service on the local network. When false it binds to the host loopback only.
//
// When the SSHAccess has a ConfigFile (Colima-style), the command uses -F to
// reference the generated SSH config and the HostAlias as the target. When
// the SSHAccess provides a direct host and key (Lume-style), the command
// connects directly with -i and user@host.
func buildForwardSSHArgs(port int, access vm.SSHAccess, bindAll bool) []string {
	bindAddr := "127.0.0.1"
	if bindAll {
		bindAddr = "0.0.0.0"
	}
	forwardSpec := fmt.Sprintf("%s:%d:localhost:%d", bindAddr, port, port)
	if access.ConfigFile != "" {
		return []string{
			"ssh",
			"-fN",
			"-L", forwardSpec,
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-F", access.ConfigFile,
			access.HostAlias,
		}
	}
	return []string{
		"ssh",
		"-fN",
		"-L", forwardSpec,
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "StrictHostKeyChecking=no",
		"-i", access.KeyFile,
		fmt.Sprintf("%s@%s", access.User, access.Host),
	}
}

// StartForward creates an SSH local port forward from the host to the VM for
// the specified port. The port must be declared in the agent configuration's
// published ports list. If a live forward already exists for the port, the
// call is a no-op and returns an error to signal the caller.
//
// When bindAll is true the SSH -L spec binds to 0.0.0.0 instead of loopback,
// making the service reachable on the local network (LAN). Use with caution.
func StartForward(profile string, port int, agentCfg *config.AgentConfig, backend vm.Backend, bindAll bool) error {
	// Ensure the requested port is declared in the agent's published ports list
	// before attempting to establish a forward.
	allowed := false
	for _, p := range agentCfg.Ports {
		if p == port {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("port %d is not in the agent's published ports %v", port, agentCfg.Ports)
	}

	stateDir, err := StateDir()
	if err != nil {
		return err
	}
	os.MkdirAll(stateDir, 0o700) //nolint:errcheck

	access := backend.SSHConfig(profile)

	// Skip startup if an existing forward process is still alive for this port.
	if pid, err := ReadForwardPID(stateDir, profile, port); err == nil && pid > 0 {
		if processAlive(pid) {
			return fmt.Errorf("port %d is already forwarded (PID %d)", port, pid)
		}
	}

	args := buildForwardSSHArgs(port, access, bindAll)

	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("starting SSH forward for port %d: %w", port, err)
	}

	// Locate the spawned daemon process and persist its PID so that a
	// subsequent DropForward call can cleanly terminate it.
	pid := findForwardPID(port, access)
	if pid > 0 {
		if err := WriteForwardPID(stateDir, profile, port, pid); err != nil {
			return fmt.Errorf("writing forward PID: %w", err)
		}
	}

	return nil
}

// DropForward tears down the SSH local forward for the given profile and port.
// It reads the stored PID, terminates the process, and removes the PID file.
func DropForward(profile string, port int) error {
	stateDir, err := StateDir()
	if err != nil {
		return err
	}

	pid, err := ReadForwardPID(stateDir, profile, port)
	if err != nil {
		return fmt.Errorf("no active forward for port %d", port)
	}

	if p, err := os.FindProcess(pid); err == nil {
		p.Kill() //nolint:errcheck
	}
	RemoveForwardPID(stateDir, profile, port)
	return nil
}

// DropAllForwards tears down every active SSH local forward for the given
// profile by iterating the stored PID files and calling DropForward for each.
func DropAllForwards(profile string) {
	stateDir, err := StateDir()
	if err != nil {
		return
	}
	for _, port := range ListForwardPorts(stateDir, profile) {
		DropForward(profile, port) //nolint:errcheck
	}
}

// processAlive returns true when the process with the given PID is present in
// the OS process table. Signal 0 is used as a non-destructive existence probe.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; signal 0 is the definitive liveness check.
	return p.Signal(syscall.Signal(0)) == nil
}

// findForwardPID locates the PID of the SSH forward daemon process by matching
// the -L forward specification and the target host/alias in the process list.
// The bind address prefix (127.0.0.1 or 0.0.0.0) is omitted from the pattern
// so that both loopback and LAN forwards are matched. It returns 0 when no
// matching process is found.
func findForwardPID(port int, access vm.SSHAccess) int {
	target := access.HostAlias
	if target == "" {
		target = access.Host
	}
	// Match the trailing :<port>:localhost:<port> portion, which is present
	// regardless of whether the bind address is loopback or 0.0.0.0.
	forwardSpec := fmt.Sprintf(":%d:localhost:%d", port, port)
	out, err := exec.Command("pgrep", "-n", "-f",
		fmt.Sprintf("ssh.*-L.*%s.*%s", forwardSpec, target),
	).Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return pid
}
