package tunnel

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/vm"
)

// AgentGridDaemonPort is the port the Agent Grid headless daemon listens on
// inside the VM. The standalone `run` command uses the daemon's default listen
// port (8765); cloister remaps a different host port rather than trying to
// relocate the guest listener.
const AgentGridDaemonPort = 8765

// AgentGridHostPort is the preferred macOS-side port for the Agent Grid
// forward. It deliberately differs from AgentGridDaemonPort because the Mac
// desktop app typically already binds *:8765, and Colima/Lima auto-forwards
// guest ports to the host on the same port number. Reusing 8765 on the host
// would collide. The +10000 offset follows the convention used elsewhere in
// cloister for host-side tunnel ports.
const AgentGridHostPort = 18765

// hostPortScanLimit bounds how far past the preferred port a search for a free
// host port will walk. Multiple profiles carrying the same stack each need
// their own host port, so a small contiguous range is reserved per service.
const hostPortScanLimit = 10

// StartLocalForward establishes an SSH local port forward (-L) from the macOS
// host to a port on the VM's loopback. This is the inverse direction of
// StartAll's reverse tunnels: it publishes a VM-side listener on the host so
// Mac clients can attach to a service running inside the VM.
//
// hostPort is exact: it is the port pinned for this profile/service, so the
// endpoint stays stable across VM restarts and paired clients keep working.
// The port is never silently relocated. When it is occupied by an unrelated
// process, StartLocalForward fails with a recovery hint instead of drifting to
// a different port. The port actually bound is recorded alongside the PID.
//
// The call is idempotent: a live PID recorded for this profile and service is
// left untouched and its recorded port returned.
func StartLocalForward(profile, name string, hostPort, vmPort int, access vm.SSHAccess) (int, error) {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return 0, fmt.Errorf("resolving tunnel state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return 0, fmt.Errorf("creating tunnel state directory: %w", err)
	}

	pidFile := localForwardPIDPath(stateDir, profile, name)
	portFile := localForwardPortPath(stateDir, profile, name)

	if pid, err := readPID(pidFile); err == nil && pid > 0 && processAlive(pid) {
		if port, ok := readPort(portFile); ok {
			return port, nil
		}
		return hostPort, nil
	}

	// Any recorded port belongs to a dead forward; drop it so a failure below
	// cannot leave a stale port on record for the next run to report.
	os.Remove(portFile) //nolint:errcheck

	port := hostPort

	// Pre-check the exact pinned port. A live forward we own was already
	// handled above, so a bind failure here means an unrelated process holds
	// the port; fail with recovery guidance rather than moving the endpoint.
	if probe, perr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); perr != nil {
		return 0, portConflictError(profile, name, port)
	} else {
		probe.Close()
	}

	forwardSpec := fmt.Sprintf("127.0.0.1:%d:localhost:%d", port, vmPort)

	// ExitOnForwardFailure makes a lost race for the local bind a hard error
	// instead of a warning on a live-but-useless SSH session. Without it ssh
	// stays up after printing "bind: Address already in use" and the caller
	// records a PID for a forward that will never carry traffic.
	args := []string{
		"-fN",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-L", forwardSpec,
	}
	if access.ConfigFile != "" {
		args = append(args, "-F", access.ConfigFile, access.HostAlias)
	} else {
		args = append(args,
			"-o", "StrictHostKeyChecking=no",
			"-i", access.KeyFile,
			fmt.Sprintf("%s@%s", access.User, access.Host))
	}

	sshCmd := exec.Command("ssh", args...)
	var sshStderr bytes.Buffer
	sshCmd.Stderr = &sshStderr
	if err := sshCmd.Run(); err != nil {
		// ExitOnForwardFailure turns a lost bind race into a non-zero exit,
		// but so does every other ssh failure (auth, unreachable VM, stale
		// ssh config). Only report a port conflict when stderr says so;
		// anything else gets the real error so the user does not chase a
		// phantom process with lsof.
		stderrText := sshStderr.String()
		if strings.Contains(stderrText, "Address already in use") ||
			strings.Contains(stderrText, "forwarding failed") ||
			strings.Contains(stderrText, "cannot listen to port") {
			return 0, portConflictError(profile, name, port)
		}
		return 0, fmt.Errorf("starting %q forward: %w: %s", name, err, strings.TrimSpace(stderrText))
	}

	searchTarget := access.Host
	if access.HostAlias != "" {
		searchTarget = access.HostAlias
	}
	pid := findLocalForwardPID(forwardSpec, searchTarget)
	if pid == 0 {
		// ssh -fN forks before the child settles into its final cmdline;
		// give it one more chance before treating the forward as lost.
		time.Sleep(200 * time.Millisecond)
		pid = findLocalForwardPID(forwardSpec, searchTarget)
	}
	if pid == 0 {
		// A live forward we cannot track would hold the pinned port and make
		// every future start misdiagnose it as a foreign process. Kill it
		// best-effort before failing.
		exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L.*%s", forwardSpec)).Run() //nolint:errcheck
		return 0, fmt.Errorf("%q forward started but its PID could not be determined", name)
	}
	if err := writePID(pidFile, pid); err != nil {
		killForwardProcess(pid)
		return 0, fmt.Errorf("writing %q forward PID file: %w", name, err)
	}
	if err := os.WriteFile(portFile, []byte(strconv.Itoa(port)+"\n"), 0o600); err != nil {
		killForwardProcess(pid)
		os.Remove(pidFile) //nolint:errcheck
		return 0, fmt.Errorf("writing %q forward port file: %w", name, err)
	}
	return port, nil
}

// killForwardProcess terminates a forward whose runtime state could not be
// recorded. An untracked forward is worse than no forward: it occupies the
// pinned port while looking like an unrelated process to the next start.
func killForwardProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// StartStackLocalForwards publishes VM-side listeners belonging to
// provisioning stacks on the host. Currently only the agentgrid stack
// participates: its daemon is reachable from Mac and phone clients over the
// forwarded port.
//
// The host port for each stack service is pinned into the profile config on
// first use (see reserveHostPort) so the endpoint stays stable across VM
// restarts and does not collide with another VM running the same stack.
func StartStackLocalForwards(cfgPath string, cfg *config.Config, profile string, backend vm.Backend) error {
	p := cfg.Profiles[profile]
	if p == nil {
		return fmt.Errorf("profile %q not found", profile)
	}

	for _, s := range p.Stacks {
		if s != "agentgrid" {
			continue
		}

		// On entry after a cold VM boot the systemd user service and its
		// Electron cold start routinely lag SSH readiness, so a single-shot
		// probe would skip the forward on exactly the restart path the port
		// pinning exists for. Poll before declaring the daemon down.
		if !agentGridDaemonReadyWait(profile, backend, 15*time.Second) {
			fmt.Printf("  ✗ agentgrid (VM port %d): daemon not listening\n", AgentGridDaemonPort)
			fmt.Printf("    check: cloister exec %s -- systemctl --user status agent-grid-daemon\n", profile)
			continue
		}

		reserved, err := reserveHostPort(cfgPath, cfg, profile, "agentgrid", AgentGridHostPort)
		if err != nil {
			return err
		}

		access := backend.SSHConfig(profile)
		port, err := StartLocalForward(profile, "agentgrid", reserved, AgentGridDaemonPort, access)
		if err != nil {
			return err
		}
		if !waitForTCP(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second) {
			return fmt.Errorf("agentgrid forward started but host port %d is unreachable", port)
		}

		label := backend.VMName(profile)
		pairOutput, pairErr := backend.SSHCommand(profile, fmt.Sprintf("agent-grid-pair --label %q", label))
		pairingCode := parseAgentGridPairingCode(pairOutput)
		if pairErr == nil && pairingCode != "" {
			fmt.Printf(
				"  ✓ agentgrid (host 127.0.0.1:%d → VM :%d), pair: 127.0.0.1:%d|%s\n",
				port,
				AgentGridDaemonPort,
				port,
				pairingCode,
			)
			continue
		}

		fmt.Printf("  ✓ agentgrid (host 127.0.0.1:%d → VM :%d)\n", port, AgentGridDaemonPort)
		fmt.Printf("    pairing code unavailable; run: cloister exec %s -- agent-grid-pair --label %s\n", profile, label)
	}
	return nil
}

func agentGridDaemonReady(profile string, backend vm.Backend) bool {
	command := fmt.Sprintf(
		"ss -ltn 2>/dev/null | grep -Eq '[:.]%d[[:space:]]'",
		AgentGridDaemonPort,
	)
	_, err := backend.SSHCommand(profile, command)
	return err == nil
}

func agentGridDaemonReadyWait(profile string, backend vm.Backend, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if agentGridDaemonReady(profile, backend) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

func waitForTCP(address string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func parseAgentGridPairingCode(output string) string {
	const prefix = "Pairing code:"

	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		code := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prefix))
		if len(code) != 6 {
			return ""
		}
		for _, char := range code {
			if (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
				return ""
			}
		}
		return code
	}

	return ""
}

// LocalForwardPort reports the host port currently recorded for a stack
// forward, and whether a live forward exists at all. Callers use it to print
// connection guidance that matches reality rather than the preferred port.
func LocalForwardPort(profile, name string) (int, bool) {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return 0, false
	}
	pid, err := readPID(localForwardPIDPath(stateDir, profile, name))
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0, false
	}
	return readPort(localForwardPortPath(stateDir, profile, name))
}

func localForwardPIDPath(stateDir, profile, name string) string {
	return filepath.Join(stateDir, fmt.Sprintf("tunnel-local-%s-%s.pid", name, profile))
}

func localForwardPortPath(stateDir, profile, name string) string {
	return filepath.Join(stateDir, fmt.Sprintf("tunnel-local-%s-%s.port", name, profile))
}

func readPort(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// reserveHostPort returns the pinned macOS-side port for a stack service,
// recording it in the profile config on first use so later restarts reuse the
// same endpoint. A port already pinned for this profile is returned as-is,
// even if it is momentarily occupied: StartLocalForward decides whether it can
// bind and reports a recovery hint otherwise. On first allocation the scan
// skips ports already reserved by other profiles so two VMs exposing the same
// service do not land on the same host port.
//
// The in-memory cfg may be minutes stale by the time this runs (profile entry
// waits on VM boot and interactive prompts first), so persisting through it
// would clobber anything another cloister process saved in the interim,
// including that process's own port pins. The reservation is therefore
// applied to a fresh copy of the config loaded from disk; the caller's cfg is
// only mirrored so subsequent reads in this process see the pin.
func reserveHostPort(cfgPath string, cfg *config.Config, profile, service string, preferred int) (int, error) {
	p := cfg.Profiles[profile]
	if p == nil {
		return 0, fmt.Errorf("profile %q not found", profile)
	}

	if p.LocalForwardPorts != nil {
		if port, ok := p.LocalForwardPorts[service]; ok && port > 0 {
			return port, nil
		}
	}

	// Re-read the config so the save below is a minimal read-modify-write.
	// When the profile is missing on disk (fresh setup where the caller has
	// not persisted it yet), fall back to saving through the caller's copy.
	target, targetProfile := cfg, p
	if fresh, err := config.Load(cfgPath); err == nil {
		if fp := fresh.Profiles[profile]; fp != nil {
			// Another process may have pinned the port since cfg was loaded;
			// adopt its reservation rather than allocating a second one.
			if port, ok := fp.LocalForwardPorts[service]; ok && port > 0 {
				mirrorReservation(p, service, port)
				return port, nil
			}
			target, targetProfile = fresh, fp
		}
	}

	reservedByOthers := make(map[int]bool)
	for name, other := range target.Profiles {
		if name == profile || other == nil {
			continue
		}
		for _, port := range other.LocalForwardPorts {
			reservedByOthers[port] = true
		}
	}

	port, err := findFreeHostPortExcluding(preferred, hostPortScanLimit, reservedByOthers)
	if err != nil {
		return 0, fmt.Errorf("selecting host port for %q forward: %w", service, err)
	}

	mirrorReservation(targetProfile, service, port)
	if err := config.Save(cfgPath, target); err != nil {
		return 0, fmt.Errorf("pinning %q host port for profile %q: %w", service, profile, err)
	}
	mirrorReservation(p, service, port)
	return port, nil
}

func mirrorReservation(p *config.Profile, service string, port int) {
	if p.LocalForwardPorts == nil {
		p.LocalForwardPorts = make(map[string]int)
	}
	p.LocalForwardPorts[service] = port
}

// portConflictError formats a hard failure for a pinned host port that an
// unrelated process holds. It preserves the reservation and tells the user how
// to recover rather than drifting the endpoint to a new port.
func portConflictError(profile, service string, port int) error {
	return fmt.Errorf(
		"host port %d for %q (profile %q) is in use by another process; the "+
			"reservation is kept so paired clients keep their endpoint. Recover by "+
			"freeing it (lsof -nP -iTCP:%d -sTCP:LISTEN, then stop that process), or "+
			"intentionally reallocate by removing local_forward_ports.%s from that "+
			"profile in ~/.cloister/config.yaml and re-entering",
		port, service, profile, port, service,
	)
}

// findFreeHostPort returns the first port in [preferred, preferred+limit) that
// accepts a loopback bind. The probe closes its listener immediately, so the
// result is advisory; ExitOnForwardFailure catches the residual race.
func findFreeHostPort(preferred, limit int) (int, error) {
	return findFreeHostPortExcluding(preferred, limit, nil)
}

// findFreeHostPortExcluding is findFreeHostPort with an additional set of ports
// treated as unavailable even when they currently accept a bind. Callers use
// it to avoid ports another profile has already reserved.
func findFreeHostPortExcluding(preferred, limit int, excluded map[int]bool) (int, error) {
	for port := preferred; port < preferred+limit && port <= 65535; port++ {
		if excluded[port] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free host port in range %d-%d", preferred, preferred+limit-1)
}

// findLocalForwardPID locates the backgrounded ssh process for a local forward
// by its forward specification, preferring a match that also names the VM.
func findLocalForwardPID(forwardSpec, vmName string) int {
	patterns := []string{
		fmt.Sprintf("ssh.*-L.*%s.*%s", forwardSpec, vmName),
		fmt.Sprintf("ssh.*-L.*%s", forwardSpec),
	}
	for _, pattern := range patterns {
		out, err := exec.Command("pgrep", "-f", pattern).Output()
		if err != nil {
			continue
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			return pid
		}
	}
	return 0
}
