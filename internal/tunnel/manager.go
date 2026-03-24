package tunnel

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/vm"
)

// dialTimeout is the maximum time allowed for a single health-check probe
// (either HTTP GET or TCP dial). Keeping this short ensures that Discover
// returns quickly even when several services are unreachable.
const dialTimeout = 500 * time.Millisecond

// DiscoveryResult pairs a built-in tunnel definition with the outcome of its
// liveness check against the macOS host.
type DiscoveryResult struct {
	// Tunnel is the static metadata for this built-in service.
	Tunnel BuiltinTunnel

	// Available is true when the health check succeeded, indicating that the
	// service is running and ready to be forwarded into a VM.
	Available bool

	// Blocked is set by FilterByPolicy when the service was detected as
	// available on the host but denied by the profile's tunnel consent policy.
	// A blocked tunnel is not forwarded into the VM.
	Blocked bool
}

// Discover probes each built-in host service and returns a DiscoveryResult for
// every entry in Builtins. Services configured with an HTTP health check are
// probed via GET; a 200 response indicates availability. Services configured
// with "tcp" are probed via a raw TCP dial to 127.0.0.1:<port>. All probes use
// a 500 ms timeout so the function returns quickly even when services are down.
func Discover() []DiscoveryResult {
	results := make([]DiscoveryResult, 0, len(Builtins))
	for _, b := range Builtins {
		results = append(results, DiscoveryResult{
			Tunnel:    b,
			Available: probe(b),
		})
	}
	return results
}

// FilterByPolicy applies a resource consent policy to discovery results.
// Tunnels that are available but denied by the policy have Available set to
// false and Blocked set to true. Tunnels that were never available are left
// unchanged. The original slice is not modified; a new slice is returned.
func FilterByPolicy(results []DiscoveryResult, policy config.ResourcePolicy) []DiscoveryResult {
	filtered := make([]DiscoveryResult, len(results))
	copy(filtered, results)
	for i := range filtered {
		if filtered[i].Available && !policy.IsAllowed(filtered[i].Tunnel.Name) {
			filtered[i].Available = false
			filtered[i].Blocked = true
		}
	}
	return filtered
}

// probe performs the health check for a single BuiltinTunnel and returns true
// when the service is considered available.
func probe(b BuiltinTunnel) bool {
	if b.HealthCheck == "tcp" {
		return probeTCP(b.Port)
	}
	return probeHTTP(b.HealthCheck)
}

// probeHTTP issues an HTTP GET to url and returns true when the response status
// is 200 OK. A custom transport with aggressive timeouts prevents the probe
// from blocking longer than dialTimeout.
func probeHTTP(url string) bool {
	client := &http.Client{
		Timeout: dialTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: dialTimeout,
			}).DialContext,
			ResponseHeaderTimeout: dialTimeout,
		},
	}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// probeTCP dials 127.0.0.1:<port> with a dialTimeout and returns true when the
// connection is accepted. The connection is closed immediately after the check.
func probeTCP(port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// StartAll establishes SSH reverse tunnels for all available built-in services
// and any additional custom tunnels from the profile configuration. It is
// idempotent: if a PID file for a tunnel already exists and the recorded
// process is still alive, the tunnel is left untouched.
//
// SSH is invoked with -fN so that it daemonises immediately after
// authentication. ControlMaster and ControlPath are disabled to ensure each
// tunnel occupies its own dedicated connection, independent of any multiplexed
// SSH sessions the user may have open.
//
// PID files are written to ~/.cloister/state/tunnel-<service>-<profile>.pid.
func StartAll(profile string, backend vm.Backend, results []DiscoveryResult, custom []config.TunnelConfig) error {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return fmt.Errorf("resolving tunnel state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating tunnel state directory: %w", err)
	}

	access := backend.SSHConfig(profile)

	for _, r := range results {
		if !r.Available {
			continue
		}
		if err := startTunnel(stateDir, profile, r.Tunnel.Name, r.Tunnel.Port, r.Tunnel.Port, access); err != nil {
			return err
		}
	}

	for _, c := range custom {
		vmPort := c.VMPort
		if vmPort == 0 {
			vmPort = c.HostPort
		}
		if err := startTunnel(stateDir, profile, c.Name, c.HostPort, vmPort, access); err != nil {
			return err
		}
	}

	return nil
}

// startTunnel ensures a single SSH reverse tunnel is running. It reads any
// existing PID file and skips startup when the recorded process is still alive.
// On success it writes a new PID file with the daemon process ID.
func startTunnel(stateDir, profile, name string, hostPort, vmPort int, access vm.SSHAccess) error {
	pidPath := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))

	// Idempotency check: skip if an existing process owns this tunnel slot.
	if pid, err := readPID(pidPath); err == nil && pid > 0 {
		if processAlive(pid) {
			return nil
		}
	}

	// -R <vmPort>:127.0.0.1:<hostPort> creates a reverse tunnel so that
	// connections to vmPort inside the VM are forwarded to hostPort on the host.
	forwardSpec := fmt.Sprintf("%d:127.0.0.1:%d", vmPort, hostPort)

	var cmd *exec.Cmd
	if access.ConfigFile != "" {
		// Colima backend: reach the VM via Lima-generated SSH config file.
		cmd = exec.Command("ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-F", access.ConfigFile, access.HostAlias)
	} else {
		// Lume backend: reach the VM via key-based auth to an mDNS hostname.
		cmd = exec.Command("ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "StrictHostKeyChecking=no",
			"-i", access.KeyFile, fmt.Sprintf("%s@%s", access.User, access.Host))
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("starting tunnel %q for profile %q: %w", name, profile, err)
	}

	// Locate the newly spawned daemon process by name and port spec so we can
	// record its PID. Failing to find it is non-fatal — the tunnel may still be
	// functional.
	searchTarget := access.HostAlias
	if searchTarget == "" {
		searchTarget = access.Host
	}
	pid := findSSHPID(forwardSpec, searchTarget)
	if pid > 0 {
		if err := writePID(pidPath, pid); err != nil {
			return fmt.Errorf("writing PID file for tunnel %q: %w", name, err)
		}
	}

	return nil
}

// StopAll terminates all SSH tunnels for the given profile by reading PID files
// from the state directory and sending SIGTERM to each recorded process. PID
// files are removed regardless of whether the kill succeeds.
func StopAll(profile string) {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return
	}

	pattern := filepath.Join(stateDir, fmt.Sprintf("tunnel-*-%s.pid", profile))
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return
	}

	for _, pidPath := range matches {
		pid, err := readPID(pidPath)
		if err == nil && pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
		_ = os.Remove(pidPath)
	}
}

// PrintDiscovery writes the discovery results to stdout using a compact status
// table. Three states are rendered:
//
//   - ✓  available and not blocked (detected on the host, will be forwarded)
//   - —  not available and blocked (detected but denied by the tunnel policy)
//   - ✗  not available and not blocked (not found on the host; install hint shown)
func PrintDiscovery(results []DiscoveryResult) {
	for _, r := range results {
		if r.Available {
			fmt.Printf("  ✓ %s (port %d)\n", r.Tunnel.Name, r.Tunnel.Port)
		} else if r.Blocked {
			fmt.Printf("  — %s (port %d) — blocked by tunnel policy\n", r.Tunnel.Name, r.Tunnel.Port)
		} else {
			fmt.Printf("  ✗ %s (port %d) — install: %s\n", r.Tunnel.Name, r.Tunnel.Port, r.Tunnel.Install)
		}
	}
}

// tunnelStateDir returns the path to the directory used for tunnel PID files,
// i.e. ~/.cloister/state.
func tunnelStateDir() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state"), nil
}

// readPID reads a PID from a file that contains a single decimal integer.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("malformed PID file %q: %w", path, err)
	}
	return pid, nil
}

// writePID writes pid as a decimal integer to path, replacing any existing
// file. The file is created with mode 0600.
func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}

// processAlive returns true when the process with the given PID exists in the
// OS process table. It sends signal 0, which performs an existence check
// without actually delivering a signal.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; signal 0 is the definitive check.
	return p.Signal(syscall.Signal(0)) == nil
}

// findSSHPID locates the PID of the ssh daemon process launched for the given
// forward specification and target host. It inspects the process list via `ps`
// to find the most recently started matching process.
func findSSHPID(forwardSpec, vmName string) int {
	out, err := exec.Command("pgrep", "-n", "-f",
		fmt.Sprintf("ssh.*-R.*%s.*%s", forwardSpec, vmName),
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

// ProbeByName checks whether the named builtin tunnel service is available
// on the host by running its health check probe.
func ProbeByName(name string) bool {
	for _, b := range Builtins {
		if b.Name == name {
			return probe(b)
		}
	}
	return false
}
