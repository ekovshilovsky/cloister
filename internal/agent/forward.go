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
// port on the host loopback to the same port on the VM's loopback via -L.
func buildForwardSSHArgs(port int, sshConfig, vmName string) []string {
	forwardSpec := fmt.Sprintf("%d:localhost:%d", port, port)
	return []string{
		"ssh",
		"-fN",
		"-L", forwardSpec,
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-F", sshConfig,
		vmName,
	}
}

// StartForward creates an SSH local port forward from the host to the VM for
// the specified port. The port must be declared in the agent configuration's
// published ports list. If a live forward already exists for the port, the
// call is a no-op and returns an error to signal the caller.
func StartForward(profile string, port int, agentCfg *config.AgentConfig) error {
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

	// Skip startup if an existing forward process is still alive for this port.
	if pid, err := ReadForwardPID(stateDir, profile, port); err == nil && pid > 0 {
		if processAlive(pid) {
			return fmt.Errorf("port %d is already forwarded (PID %d)", port, pid)
		}
	}

	sshConfig := vm.SSHConfig(profile)
	vmName := vm.SSHHost(profile)
	args := buildForwardSSHArgs(port, sshConfig, vmName)

	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("starting SSH forward for port %d: %w", port, err)
	}

	// Locate the spawned daemon process and persist its PID so that a
	// subsequent CloseForward call can cleanly terminate it.
	pid := findForwardPID(port, vmName)
	if pid > 0 {
		if err := WriteForwardPID(stateDir, profile, port, pid); err != nil {
			return fmt.Errorf("writing forward PID: %w", err)
		}
	}

	return nil
}

// CloseForward tears down the SSH local forward for the given profile and port.
// It reads the stored PID, terminates the process, and removes the PID file.
func CloseForward(profile string, port int) error {
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

// CloseAllForwards tears down every active SSH local forward for the given
// profile by iterating the stored PID files and calling CloseForward for each.
func CloseAllForwards(profile string) {
	stateDir, err := StateDir()
	if err != nil {
		return
	}
	for _, port := range ListForwardPorts(stateDir, profile) {
		CloseForward(profile, port) //nolint:errcheck
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
// the -L forward specification and target VM name in the process list. It
// returns 0 when no matching process is found.
func findForwardPID(port int, vmName string) int {
	forwardSpec := fmt.Sprintf("%d:localhost:%d", port, port)
	out, err := exec.Command("pgrep", "-n", "-f",
		fmt.Sprintf("ssh.*-L.*%s.*%s", forwardSpec, vmName),
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
