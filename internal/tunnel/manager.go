package tunnel

import (
	"context"
	"encoding/json"
	"errors"
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

	"cloister.io/internal/config"
	"cloister.io/internal/processidentity"
	"cloister.io/internal/vm"
)

const ownedReverseForwardStartTimeout = 12 * time.Second

// ReverseForwardOwner is the complete identity of one service-owned tunnel.
type ReverseForwardOwner struct {
	OwnerID         string                   `json:"owner_id"`
	PID             int                      `json:"pid"`
	ProcessIdentity processidentity.Identity `json:"process_identity"`
	HostPort        int                      `json:"host_port"`
	GuestPort       int                      `json:"guest_port"`
	Target          string                   `json:"target"`
}

type tunnelProcessRecord struct {
	PID      int                      `json:"pid"`
	Identity processidentity.Identity `json:"process_identity"`
}

var legacyProcessIdentityRead = processidentity.Read
var legacyProcessCommand = processCommand
var legacyProcessParentPID = func(pid int) (int, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(output)))
}
var legacyProcessKill = processidentity.Kill

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
// behavior of Discover. Builtins gated by a flag (e.g. "GPGSigning") are
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
// Unknown or unrecognized flag names return false rather than panicking, which
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

// resolveGuestSocket substitutes "$HOME" and "$UID" placeholders in template
// against the VM's actual values, resolved via one-shot SSH commands. When
// template references neither placeholder it is returned unchanged so the
// function stays a no-op for fully-absolute paths. Both placeholders may
// appear in the same template; SSH calls are skipped for placeholders that
// are absent.
func resolveGuestSocket(profile string, backend vm.Backend, template string) (string, error) {
	resolved := template
	if strings.Contains(resolved, "$HOME") {
		home, err := resolveGuestValue(profile, backend, `"$HOME"`)
		if err != nil {
			return "", fmt.Errorf("resolving VM home directory: %w", err)
		}
		if home == "" {
			return "", fmt.Errorf("empty $HOME from VM")
		}
		resolved = strings.ReplaceAll(resolved, "$HOME", home)
	}
	if strings.Contains(resolved, "$UID") {
		uid, err := resolveGuestValue(profile, backend, `"$(id -u)"`)
		if err != nil {
			return "", fmt.Errorf("resolving VM uid: %w", err)
		}
		if uid == "" {
			return "", fmt.Errorf("empty uid from VM")
		}
		resolved = strings.ReplaceAll(resolved, "$UID", uid)
	}
	return resolved, nil
}

// resolveGuestValue evaluates a value-producing shell expression in the guest
// and returns the trimmed result. It uses the capture-only stdin path because
// commands passed to SSHCommand do not survive colima's argument reconstruction
// after --, and because the sentinel-wrapped value must not be streamed to the
// user's terminal. The value is wrapped in sentinels so a login-shell banner on
// stdout cannot corrupt the parsed result.
func resolveGuestValue(profile string, backend vm.Backend, valueExpr string) (string, error) {
	out, err := backend.SSHCapture(profile, `printf '__CLV[%s]CLV__' `+valueExpr)
	if err != nil {
		return "", err
	}
	start := strings.Index(out, "__CLV[")
	end := strings.Index(out, "]CLV__")
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("unexpected guest output %q", out)
	}
	return strings.TrimSpace(out[start+len("__CLV[") : end]), nil
}

// startTunnel ensures a single SSH reverse tunnel is running. It reads any
// existing process record and skips startup only when its PID and kernel start
// time match. An unreadable live identity blocks competing startup.
func startTunnel(stateDir, profile, name string, hostPort, vmPort int, access vm.SSHAccess) error {
	pidPath := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))

	// Idempotency check: skip if an existing process owns this tunnel slot.
	if running, err := existingTunnelProcessState(pidPath); err != nil {
		return err
	} else if running {
		return nil
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

func existingTunnelProcessState(path string) (bool, error) {
	if record, err := readTunnelProcessRecord(path); err == nil {
		switch observation := processidentity.Observe(record.PID, record.Identity); observation.State {
		case processidentity.Ours:
			return true, nil
		case processidentity.Unverifiable:
			return false, fmt.Errorf("tunnel PID %d is alive but its ownership cannot be verified: %w; refusing competing startup", record.PID, observation.Err)
		default:
			_ = os.Remove(path)
			return false, nil
		}
	}
	pid, err := readPID(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading tunnel process record: %w", err)
	}
	if processidentity.Observe(pid, processidentity.Identity{}).State == processidentity.Dead {
		_ = os.Remove(path)
		return false, nil
	}
	return false, fmt.Errorf("tunnel PID %d is alive without a recorded kernel start time; refusing competing startup", pid)
}

// StartOwnedReverseForward follows the existing detached ssh tunnel pattern,
// but records enough identity for safe service-owned teardown. It never kills
// an existing owned tunnel.
func StartOwnedReverseForward(profile, name, ownerID string, hostPort, guestPort int, access vm.SSHAccess) (ReverseForwardOwner, error) {
	if ownerID == "" || strings.ContainsAny(ownerID, "\r\n") || hostPort <= 0 || hostPort > 65535 || guestPort <= 0 || guestPort > 65535 {
		return ReverseForwardOwner{}, fmt.Errorf("invalid owned reverse forward")
	}
	stateDir, err := tunnelStateDir()
	if err != nil {
		return ReverseForwardOwner{}, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return ReverseForwardOwner{}, fmt.Errorf("creating tunnel state directory: %w", err)
	}
	target := access.HostAlias
	if target == "" {
		target = fmt.Sprintf("%s@%s", access.User, access.Host)
	}
	ownerPath := ownedReverseForwardPath(stateDir, profile, name)
	if current, readErr := readReverseForwardOwner(ownerPath); readErr == nil {
		switch observation := processidentity.Observe(current.PID, current.ProcessIdentity); observation.State {
		case processidentity.Ours:
			return ReverseForwardOwner{}, fmt.Errorf("owned tunnel %q for profile %q is already running", name, profile)
		case processidentity.Unverifiable:
			return ReverseForwardOwner{}, fmt.Errorf("cannot verify owned tunnel PID %d: %w; refusing to start a competing tunnel", current.PID, observation.Err)
		}
		_ = os.Remove(ownerPath)
	}

	// The profile manager must run the narrowly scoped legacy migration before
	// service startup. A direct daemon invocation cannot discard this record.
	legacyPath := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))
	if _, readErr := os.Stat(legacyPath); readErr == nil {
		return ReverseForwardOwner{}, fmt.Errorf("legacy tunnel record %q has not been migrated", legacyPath)
	} else if !os.IsNotExist(readErr) {
		return ReverseForwardOwner{}, fmt.Errorf("checking legacy tunnel record: %w", readErr)
	}

	forwardSpec := fmt.Sprintf("%d:127.0.0.1:%d", guestPort, hostPort)
	ctx, cancel := context.WithTimeout(context.Background(), ownedReverseForwardStartTimeout)
	defer cancel()
	var command *exec.Cmd
	if access.ConfigFile != "" {
		command = exec.CommandContext(ctx, "ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "ExitOnForwardFailure=yes", "-F", access.ConfigFile, access.HostAlias)
	} else {
		command = exec.CommandContext(ctx, "ssh", "-fN", "-R", forwardSpec,
			"-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=no",
			"-i", access.KeyFile, fmt.Sprintf("%s@%s", access.User, access.Host))
	}
	if err := command.Run(); err != nil {
		return ReverseForwardOwner{}, fmt.Errorf("starting owned tunnel %q for profile %q: %w", name, profile, err)
	}
	pid := findSSHPID(forwardSpec, target)
	if pid <= 0 {
		return ReverseForwardOwner{}, fmt.Errorf("locating owned tunnel %q for profile %q", name, profile)
	}
	identity, err := processidentity.Read(pid)
	if err != nil {
		return ReverseForwardOwner{}, fmt.Errorf("capturing owned tunnel process identity: %w", err)
	}
	owner := ReverseForwardOwner{OwnerID: ownerID, PID: pid, ProcessIdentity: identity, HostPort: hostPort, GuestPort: guestPort, Target: target}
	if err := writeReverseForwardOwner(ownerPath, owner); err != nil {
		_ = processidentity.Kill(pid, identity)
		return ReverseForwardOwner{}, err
	}
	return owner, nil
}

// OwnedReverseForwardHealthy verifies the ownership record, kernel process
// identity. Endpoint health is checked through the guest. SSH command text is
// diagnostic only because kernel start time is the ownership authority.
func OwnedReverseForwardHealthy(profile, name string, expected ReverseForwardOwner) bool {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return false
	}
	current, err := readReverseForwardOwner(ownedReverseForwardPath(stateDir, profile, name))
	return err == nil && current == expected && reverseForwardProcessMatches(current)
}

// OwnedReverseForwardProcessState classifies the exact recorded tunnel PID.
// Missing or superseded claims are dead from this generation's perspective;
// an unreadable live PID remains unverifiable and its record is retained.
func OwnedReverseForwardProcessState(profile, name string, expected ReverseForwardOwner) processidentity.Observation {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return processidentity.Observation{State: processidentity.Unverifiable, Err: err}
	}
	current, err := readReverseForwardOwner(ownedReverseForwardPath(stateDir, profile, name))
	if os.IsNotExist(err) || (err == nil && current != expected) {
		return processidentity.Observation{State: processidentity.Dead}
	}
	if err != nil {
		return processidentity.Observation{State: processidentity.Unverifiable, Err: err}
	}
	return processidentity.Observe(current.PID, current.ProcessIdentity)
}

// StopOwnedReverseForward acts only on an exact current claim whose kernel
// start time still matches. Unverifiable processes are not signaled.
func StopOwnedReverseForward(profile, name string, expected ReverseForwardOwner) bool {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return false
	}
	path := ownedReverseForwardPath(stateDir, profile, name)
	return stopOwnedReverseForwardAtPath(path, expected)
}

func stopOwnedReverseForwardAtPath(path string, expected ReverseForwardOwner) bool {
	current, err := readReverseForwardOwner(path)
	if err != nil || current != expected {
		return false
	}
	switch observation := processidentity.Observe(current.PID, current.ProcessIdentity); observation.State {
	case processidentity.Ours:
		if processidentity.Kill(current.PID, current.ProcessIdentity) != nil {
			return false
		}
	case processidentity.Unverifiable:
		return false
	}
	_ = os.Remove(path)
	return true
}

func ownedReverseForwardPath(stateDir, profile, name string) string {
	return filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.owner.json", name, profile))
}

func readReverseForwardOwner(path string) (ReverseForwardOwner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReverseForwardOwner{}, err
	}
	var owner ReverseForwardOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return ReverseForwardOwner{}, err
	}
	return owner, nil
}

func writeReverseForwardOwner(path string, owner ReverseForwardOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".owned-tunnel-*")
	if err != nil {
		return fmt.Errorf("creating owned tunnel state: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func reverseForwardProcessMatches(owner ReverseForwardOwner) bool {
	return owner.PID > 0 && owner.Target != "" && processidentity.Observe(owner.PID, owner.ProcessIdentity).State == processidentity.Ours
}

func legacyReverseForwardMatches(pid, guestPort int, target string) bool {
	_, matched := legacyReverseForwardIdentity(pid, guestPort, target)
	return matched
}

func legacyReverseForwardIdentity(pid, guestPort int, target string) (processidentity.Identity, bool) {
	identity, err := legacyProcessIdentityRead(pid)
	if err != nil || filepath.Base(identity.Executable) != "ssh" {
		return processidentity.Identity{}, false
	}
	parentPID, err := legacyProcessParentPID(pid)
	if err != nil || parentPID != 1 {
		return processidentity.Identity{}, false
	}
	command, err := legacyProcessCommand(pid)
	if err != nil {
		return processidentity.Identity{}, false
	}
	fields := strings.Fields(command)
	forwardPrefix := fmt.Sprintf("%d:127.0.0.1:", guestPort)
	hasForward := false
	for i, field := range fields {
		if field == "-R" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], forwardPrefix) {
			hostPort, portErr := strconv.Atoi(strings.TrimPrefix(fields[i+1], forwardPrefix))
			hasForward = portErr == nil && hostPort > 0 && hostPort <= 65535
		}
	}
	return identity, hasForward && commandHasExecutable(fields, "ssh") && commandHasField(fields, "-fN") && commandHasField(fields, target)
}

// RetireLegacyReverseForward performs the one-time migration from the released
// integer-only detached tunnel record. It captures a kernel identity and only
// signals a PPID-1 ssh process with the exact legacy reverse-forward shape and
// this profile's SSH destination. It reports whether it retired a live tunnel.
func RetireLegacyReverseForward(profile, name string, guestPort int, access vm.SSHAccess) (bool, error) {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("legacy tunnel record %q is not an integer PID", path)
	}
	if processidentity.Observe(pid, processidentity.Identity{}).State == processidentity.Dead {
		return false, os.Remove(path)
	}
	target := access.HostAlias
	if target == "" {
		target = fmt.Sprintf("%s@%s", access.User, access.Host)
	}
	identity, matched := legacyReverseForwardIdentity(pid, guestPort, target)
	if !matched {
		return false, fmt.Errorf("legacy VCS tunnel PID %d is alive but does not match the narrowly scoped migration identity; refusing to signal or replace it", pid)
	}
	command, commandErr := legacyProcessCommand(pid)
	if commandErr != nil || !legacyReverseForwardAccessMatches(strings.Fields(command), access) {
		return false, fmt.Errorf("legacy VCS tunnel PID %d does not use this profile's SSH access; refusing to signal or replace it", pid)
	}
	if err := legacyProcessKill(pid, identity); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("retiring legacy VCS tunnel PID %d: %w", pid, err)
	}
	return true, os.Remove(path)
}

func legacyReverseForwardAccessMatches(fields []string, access vm.SSHAccess) bool {
	if access.ConfigFile != "" {
		return adjacentCommandFields(fields, "-F", access.ConfigFile) && commandHasField(fields, access.HostAlias)
	}
	return adjacentCommandFields(fields, "-i", access.KeyFile) && commandHasField(fields, fmt.Sprintf("%s@%s", access.User, access.Host))
}

func adjacentCommandFields(fields []string, first, second string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == first && fields[i+1] == second {
			return true
		}
	}
	return false
}

func processCommand(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(out)), err
}

func commandHasField(fields []string, expected string) bool {
	for _, field := range fields {
		if field == expected {
			return true
		}
	}
	return false
}

func commandHasFieldPrefix(fields []string, prefix string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

func commandHasExecutable(fields []string, name string) bool {
	for _, field := range fields {
		if filepath.Base(field) == name {
			return true
		}
	}
	return false
}

// StopAll terminates session-managed SSH tunnels for the given profile. The
// standalone VCS broker's owned reverse forward is excluded: its daemon must
// drain accepted commands before lifecycle code stops that exact claim.
func StopAll(profile string) {
	stateDir, err := tunnelStateDir()
	if err != nil {
		return
	}

	pattern := filepath.Join(stateDir, fmt.Sprintf("tunnel-*-%s.pid", profile))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	for _, pidPath := range matches {
		if filepath.Base(pidPath) == fmt.Sprintf("tunnel-vcs-broker-%s.pid", profile) {
			// Broker lifecycle owns both current claims and legacy PID records.
			// A generic tunnel sweep must never bypass the broker's drain.
			continue
		}
		record, err := readTunnelProcessRecord(pidPath)
		if err != nil {
			pid, pidErr := readPID(pidPath)
			if pidErr == nil && processidentity.Observe(pid, processidentity.Identity{}).State == processidentity.Dead {
				_ = os.Remove(pidPath)
			}
			continue
		}
		switch observation := processidentity.Observe(record.PID, record.Identity); observation.State {
		case processidentity.Ours:
			if processidentity.Kill(record.PID, record.Identity) != nil {
				continue
			}
		case processidentity.Unverifiable:
			continue
		}
		_ = os.Remove(pidPath)
	}

	ownedPattern := filepath.Join(stateDir, fmt.Sprintf("tunnel-*-%s.owner.json", profile))
	if owned, globErr := filepath.Glob(ownedPattern); globErr == nil {
		for _, path := range owned {
			if path == ownedReverseForwardPath(stateDir, profile, "vcs-broker") {
				continue
			}
			claim, readErr := readReverseForwardOwner(path)
			if readErr == nil {
				stopOwnedReverseForwardAtPath(path, claim)
			} else {
				_ = os.Remove(path)
			}
		}
	}

	// Local forwards also record the bound host port next to their PID file.
	// That is runtime state tied to the killed process, so drop it alongside;
	// the durable reservation lives in config.yaml and is never touched here.
	portPattern := filepath.Join(stateDir, fmt.Sprintf("tunnel-*-%s.port", profile))
	if portFiles, err := filepath.Glob(portPattern); err == nil {
		for _, portPath := range portFiles {
			_ = os.Remove(portPath)
		}
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

// readPID reads either a current identity-bearing record or a legacy integer.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var record tunnelProcessRecord
	if json.Unmarshal(data, &record) == nil && record.PID > 0 {
		return record.PID, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("malformed PID file %q: %w", path, err)
	}
	return pid, nil
}

func readTunnelProcessRecord(path string) (tunnelProcessRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return tunnelProcessRecord{}, err
	}
	var record tunnelProcessRecord
	if err := json.Unmarshal(data, &record); err != nil || record.PID <= 0 || record.Identity.StartTime == "" {
		return tunnelProcessRecord{}, fmt.Errorf("tunnel process record %q lacks PID-reuse-safe identity", path)
	}
	return record, nil
}

// writePID records PID-reuse-safe kernel identity at the existing PID path.
func writePID(path string, pid int) error {
	identity, err := processidentity.Read(pid)
	if err != nil {
		return err
	}
	data, err := json.Marshal(tunnelProcessRecord{PID: pid, Identity: identity})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func tunnelProcessRecordMatchesSSH(record tunnelProcessRecord) bool {
	return record.PID > 0 && processidentity.Matches(record.PID, record.Identity)
}

// processAlive is a liveness observation only. It never authorizes a signal;
// teardown requires processidentity.Matches against captured kernel metadata.
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
	// The host socket exists only while its backing daemon runs. For
	// gpg-forward the daemon is the host gpg-agent, which exits when idle and
	// takes its extra-socket with it; launch it on demand so VM entry is
	// self-healing instead of silently starting a hollow tunnel.
	if err := ensureHostSocket(hostSocket, launchHostGPGAgent); err != nil {
		return err
	}

	stateDir, err := tunnelStateDir()
	if err != nil {
		return fmt.Errorf("resolving tunnel state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("creating tunnel state directory: %w", err)
	}

	pidPath := filepath.Join(stateDir, fmt.Sprintf("tunnel-%s-%s.pid", name, profile))
	if record, err := readTunnelProcessRecord(pidPath); err == nil && tunnelProcessRecordMatchesSSH(record) {
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
