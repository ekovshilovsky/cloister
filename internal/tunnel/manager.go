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
// with "tcp" are probed via a raw TCP dial to 127.0.0.1:<port>. Services
// configured with "socket" are probed by stat-ing the resolved host socket
// path. All probes use a 500 ms timeout so the function returns quickly even
// when services are down.
//
// Discover does not consider RequiresFlag gating; for profile-aware discovery
// (which omits flag-gated builtins when the flag is not set on the profile),
// see DiscoverForProfile.
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

// DiscoverForProfile probes built-in host services and returns DiscoveryResult
// entries for those whose RequiresFlag (if any) is satisfied by the given
// profile. Builtins with no RequiresFlag are always probed, preserving the
// behaviour of Discover. Builtins gated by a flag (e.g. "GPGSigning") are
// skipped entirely when the profile has the flag unset, so they neither
// generate console noise nor occupy a slot in the discovery list.
func DiscoverForProfile(p *config.Profile) []DiscoveryResult {
	results := make([]DiscoveryResult, 0, len(Builtins))
	for _, b := range Builtins {
		if b.RequiresFlag != "" && !profileFlag(p, b.RequiresFlag) {
			continue
		}
		results = append(results, DiscoveryResult{
			Tunnel:    b,
			Available: probe(b),
		})
	}
	return results
}

// profileFlag returns the boolean value of a named feature flag on the profile.
// It centralises the mapping from RequiresFlag string identifiers to typed
// fields on config.Profile so the registry stays decoupled from config layout.
// Unknown or unrecognised flag names return false rather than panicking, which
// gates the corresponding builtin off until the registry is taught about the
// new flag.
func profileFlag(p *config.Profile, name string) bool {
	if p == nil {
		return false
	}
	switch name {
	case "GPGSigning":
		return p.GPGSigning
	}
	return false
}

// FilterByPolicy applies a resource consent policy to discovery results.
// Tunnels that are available but denied by the policy have Available set to
// false and Blocked set to true. Tunnels that were never available are left
// unchanged. The original slice is not modified; a new slice is returned.
//
// Builtins gated by a feature flag (RequiresFlag) bypass the policy check.
// DiscoverForProfile only emits a flag-gated entry when the corresponding
// profile flag is set, so by the time such an entry reaches FilterByPolicy
// the user has already opted in via that flag. Requiring an additional
// tunnel_policy entry would add friction without any security benefit, since
// the flag itself is the consent signal for forwarding the underlying socket.
func FilterByPolicy(results []DiscoveryResult, policy config.ResourcePolicy) []DiscoveryResult {
	filtered := make([]DiscoveryResult, len(results))
	copy(filtered, results)
	for i := range filtered {
		if filtered[i].Tunnel.RequiresFlag != "" {
			// Flag-gated builtins are implicitly consented via the feature
			// flag, so skip the deny-check entirely.
			continue
		}
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
	switch b.HealthCheck {
	case "tcp":
		return probeTCP(b.Port)
	case "socket":
		return probeSocket(b)
	}
	return probeHTTP(b.HealthCheck)
}

// probeSocket resolves the host-side socket path through the builtin's
// resolver and confirms the resulting path exists and is a Unix-domain socket.
// A nil resolver, a resolver error, an empty path, or a non-socket file all
// yield false so the builtin is treated as unavailable rather than being
// erroneously forwarded to a missing or wrong-type endpoint.
func probeSocket(b BuiltinTunnel) bool {
	if b.HostSocketResolver == nil {
		return false
	}
	path, err := b.HostSocketResolver()
	if err != nil || path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
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
		if r.Tunnel.HealthCheck == "socket" {
			hostSocket, err := r.Tunnel.HostSocketResolver()
			if err != nil {
				// Resolver reachability is already covered by probeSocket, so
				// reaching this branch indicates a transient host-side issue
				// (e.g. state file removed between Discover and StartAll).
				// Log and continue with other tunnels rather than aborting the
				// whole batch; the user can re-run setup to recover.
				fmt.Fprintf(os.Stderr, "warning: skipping %q tunnel: %v\n", r.Tunnel.Name, err)
				continue
			}
			guestSocket, err := resolveGuestSocket(profile, backend, r.Tunnel.GuestSocket)
			if err != nil {
				return fmt.Errorf("resolving guest socket for %q: %w", r.Tunnel.Name, err)
			}
			if err := StartSocketTunnel(profile, r.Tunnel.Name, guestSocket, hostSocket, access); err != nil {
				return err
			}
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

// resolveGuestSocket substitutes a "$HOME" placeholder in template against
// the VM's actual home directory, resolved via a one-shot SSH command. When
// template does not reference $HOME it is returned unchanged so the function
// stays a no-op for absolute paths.
func resolveGuestSocket(profile string, backend vm.Backend, template string) (string, error) {
	if !strings.Contains(template, "$HOME") {
		return template, nil
	}
	out, err := backend.SSHCommand(profile, "echo $HOME")
	if err != nil {
		return "", fmt.Errorf("resolving VM home directory: %w", err)
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("empty $HOME from VM")
	}
	return strings.ReplaceAll(template, "$HOME", home), nil
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
//
// Socket-style tunnels (Port == 0) render with "(socket)" instead of the
// numeric port label since the socket path is host-specific and not user-facing.
func PrintDiscovery(results []DiscoveryResult) {
	for _, r := range results {
		label := fmt.Sprintf("port %d", r.Tunnel.Port)
		if r.Tunnel.Port == 0 {
			label = "socket"
		}
		if r.Available {
			fmt.Printf("  ✓ %s (%s)\n", r.Tunnel.Name, label)
		} else if r.Blocked {
			fmt.Printf("  — %s (%s) — blocked by tunnel policy\n", r.Tunnel.Name, label)
		} else {
			fmt.Printf("  ✗ %s (%s) — install: %s\n", r.Tunnel.Name, label, r.Tunnel.Install)
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

// StartSocketTunnel establishes a single SSH reverse tunnel that forwards a
// Unix-domain socket from the host into a VM. It mirrors startTunnel's
// idempotency, ControlMaster=no posture, and PID-file conventions, but uses
// the OpenSSH "<remote-socket>:<local-socket>" form of -R instead of the
// TCP <port>:host:<port> form.
//
// The function returns an error before invoking ssh if hostSocket does not
// exist or is not a socket on the host filesystem. Callers should treat that
// error as recoverable: log a warning and continue without the tunnel.
//
// PID files are written to ~/.cloister/state/tunnel-<name>-<profile>.pid so
// that StopAll picks them up via the same glob it uses for TCP tunnels.
func StartSocketTunnel(profile, name, guestSocket, hostSocket string, access vm.SSHAccess) error {
	fi, err := os.Stat(hostSocket)
	if err != nil {
		return fmt.Errorf("host socket %q not reachable: %w", hostSocket, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("host socket %q is not a socket", hostSocket)
	}

	stateDir, err := tunnelStateDir()
	if err != nil {
		return fmt.Errorf("resolving tunnel state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating tunnel state directory: %w", err)
	}

	pidPath := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))
	if pid, err := readPID(pidPath); err == nil && pid > 0 && processAlive(pid) {
		return nil
	}

	// -R <guestSocket>:<hostSocket> forwards a Unix-domain socket inside the VM
	// to the corresponding host socket. ExitOnForwardFailure ensures ssh exits
	// non-zero if the remote bind fails (e.g. stale socket, permission denied),
	// so callers see a clean failure instead of a silently broken tunnel.
	forwardSpec := fmt.Sprintf("%s:%s", guestSocket, hostSocket)

	var cmd *exec.Cmd
	if access.ConfigFile != "" {
		// Colima backend: reach the VM via the Lima-generated SSH config file.
		cmd = exec.Command("ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "ExitOnForwardFailure=yes",
			"-F", access.ConfigFile, access.HostAlias)
	} else {
		// Lume backend: reach the VM via key-based auth to an mDNS hostname.
		cmd = exec.Command("ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "ExitOnForwardFailure=yes",
			"-o", "StrictHostKeyChecking=no",
			"-i", access.KeyFile, fmt.Sprintf("%s@%s", access.User, access.Host))
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("starting socket tunnel %q for profile %q: %w", name, profile, err)
	}

	// Locate the daemonised ssh process so its PID can be recorded for later
	// teardown. A zero result is non-fatal: the tunnel may still be functional
	// even if the lookup fails to match (e.g. on systems where pgrep is absent).
	searchTarget := access.HostAlias
	if searchTarget == "" {
		searchTarget = access.Host
	}
	pid := findSSHPID(forwardSpec, searchTarget)
	if pid > 0 {
		if err := writePID(pidPath, pid); err != nil {
			return fmt.Errorf("writing PID file for socket tunnel %q: %w", name, err)
		}
	}

	return nil
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
