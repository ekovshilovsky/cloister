package lume

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"cloister.io/internal/vm"
)

// Backend implements vm.Backend and vm.NATNetworker by wrapping the Lume CLI.
// Each method translates a cloister profile name into the corresponding Lume VM
// identifier and delegates to the appropriate `lume` sub-command. SSH
// connectivity is established using per-profile Ed25519 keys managed by keys.go
// and the VM's mDNS hostname in the ".local" domain.
type Backend struct {
	// verbose is configured before the backend is used. Backend operations may
	// run concurrently after that, but changing verbosity concurrently with an
	// operation is intentionally unsupported.
	verbose bool
}

// SetVerbose controls connection-target diagnostics for SSH operations.
func (b *Backend) SetVerbose(verbose bool) {
	b.verbose = verbose
}

// lumeVM is the JSON structure emitted by `lume get <name> --format json` and
// by individual entries in `lume ls --format json`. Only the fields consumed
// by cloister are declared; additional fields returned by Lume are silently
// ignored.
type lumeVM struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	CPUs       int       `json:"cpuCount"`
	MemorySize int64     `json:"memorySize"`
	DiskSize   lumeDisk  `json:"diskSize"`
	Arch       string    `json:"arch"`
	IP         string    `json:"ipAddress"`
	OS         string    `json:"os"`
	Created    time.Time `json:"created"`
}

type lumeDisk struct {
	Total     int64 `json:"total"`
	Allocated int64 `json:"allocated"`
}

// memoryGB returns the memory in gigabytes for vm.VMStatus compatibility.
func (v lumeVM) memoryGB() int {
	return int(v.MemorySize / (1024 * 1024 * 1024))
}

// diskGB returns the total disk in gigabytes for vm.VMStatus compatibility.
func (v lumeVM) diskGB() int {
	return int(v.DiskSize.Total / (1024 * 1024 * 1024))
}

// Start creates or resumes the Lume VM for the given profile with the specified
// resource allocation. The VM is launched in headless mode via --no-display.
// Each entry in mounts is supplied as a --shared-dir flag; writable mounts
// pass the path alone and read-only mounts append ":ro". When verbose is true,
// Lume's output is forwarded to stderr.
//
// Note: resource fields in StartSpec are accepted for interface compliance but
// are not applied at run time. Lume configures resources at
// creation via `lume set`, not at boot via `lume run`. The createLumeProfile
// flow calls `lume set` before Start to apply the desired resources. Lume
// uses a single APFS-based disk image with no separate root/data partition
// concept, so rootDiskGB is unused; the field is part of the interface to
// support Colima's two-disk model.
func (b *Backend) Start(profile string, spec vm.StartSpec) error {
	cleanStaleLumeProcesses()

	args, err := startArgs(profile, spec)
	if err != nil {
		return err
	}
	name := VMName(profile)

	var stderrBuf bytes.Buffer
	cmd := exec.Command("lume", args...)
	cmd.Stderr = &stderrBuf
	if spec.Verbose {
		cmd.Stdout = os.Stderr
		cmd.Stderr = &teeWriter{buf: &stderrBuf, w: os.Stderr}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lume run %s: %w", name, err)
	}

	// lume run blocks until the VM stops. Monitor for early exit (within
	// 5 seconds) which indicates a startup failure — surface the error
	// immediately instead of waiting 120 seconds in waitForLumeReady.
	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()

	select {
	case err := <-errCh:
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "lume run %s stderr:\n%s\n", name, stderr)
		}
		if err != nil {
			return fmt.Errorf("lume run %s failed immediately: %w\n%s", name, err, stderr)
		}
		return fmt.Errorf("lume run %s exited unexpectedly with no error", name)
	case <-time.After(5 * time.Second):
		// VM is still running after 5s — it started successfully.
		return nil
	}
}

func startArgs(profile string, spec vm.StartSpec) ([]string, error) {
	mounts, err := spec.Mounts()
	if err != nil {
		return nil, fmt.Errorf("lume start %s: %w", VMName(profile), err)
	}
	args := []string{"run", VMName(profile), "--no-display"}
	for _, m := range mounts {
		if m.MountPoint != "" && m.MountPoint != m.Location {
			return nil, fmt.Errorf("lume start %s: relocated mount %q to %q is unsupported", VMName(profile), m.Location, m.MountPoint)
		}
		sharedDir := m.Location
		if !m.Writable {
			sharedDir += ":ro"
		}
		args = append(args, "--shared-dir", sharedDir)
	}
	return args, nil
}

// cleanStaleLumeProcesses checks for processes that may be consuming macOS
// Virtualization.framework VM slots (limit: 2 active VMs). It reports stale
// `lume run` processes whose VMs are no longer running, and warns about other
// VZ consumers like Docker Desktop. Processes are not killed automatically —
// the user is told what to do.
func cleanStaleLumeProcesses() {
	out, _ := exec.Command("pgrep", "-f", "lume.*run").Output()
	pids := strings.Fields(strings.TrimSpace(string(out)))

	for _, pidStr := range pids {
		pid := 0
		fmt.Sscanf(pidStr, "%d", &pid)
		if pid == 0 || pid == os.Getpid() {
			continue
		}

		cmdline, err := exec.Command("ps", "-p", pidStr, "-o", "command=").Output()
		if err != nil {
			continue
		}
		cmd := strings.TrimSpace(string(cmdline))

		parts := strings.Fields(cmd)
		vmName := ""
		for i, p := range parts {
			if p == "run" && i+1 < len(parts) {
				vmName = parts[i+1]
				break
			}
		}
		if vmName == "" {
			continue
		}

		v, err := lumeGetVM(vmName)
		if err != nil || !strings.EqualFold(v.Status, "running") {
			fmt.Fprintf(os.Stderr, "Warning: stale lume process (pid %d) for VM %q which is not running.\n", pid, vmName)
			fmt.Fprintf(os.Stderr, "  This holds a macOS VM slot. Run: cloister cleanup\n")
		}
	}

	vzOut, _ := exec.Command("pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
	vzCount := len(strings.Fields(strings.TrimSpace(string(vzOut))))
	if vzCount >= 2 {
		fmt.Fprintf(os.Stderr, "Warning: %d Virtualization.framework VMs active (macOS limit is 2). Run: cloister cleanup\n", vzCount)
	}
}

// Stop gracefully shuts down the running VM for the given profile. When verbose
// is true, Lume's combined output is forwarded to the terminal.
func (b *Backend) Stop(profile string, verbose bool) error {
	name := VMName(profile)
	if err := runLume(verbose, "stop", name); err != nil {
		return fmt.Errorf("lume stop %s: %w", name, err)
	}
	return nil
}

// Delete permanently destroys the VM for the given profile and releases all
// associated resources. The --force flag allows deletion regardless of the
// current VM state. When verbose is true, Lume's combined output is forwarded
// to the terminal.
func (b *Backend) Delete(profile string, verbose bool) error {
	name := VMName(profile)
	if err := runLume(verbose, "delete", name, "--force"); err != nil {
		return fmt.Errorf("lume delete %s: %w", name, err)
	}
	return nil
}

// Exists reports whether a Lume VM for the given profile is registered,
// regardless of whether it is currently running. It uses `lume get` with JSON
// output as a lightweight existence probe.
func (b *Backend) Exists(profile string) bool {
	name := VMName(profile)
	cmd := exec.Command("lume", "get", name, "--format", "json")
	return cmd.Run() == nil
}

// IsRunning reports whether the VM for the given profile is currently in the
// running state. It returns false when the VM does not exist or cannot be
// queried.
func (b *Backend) IsRunning(profile string) bool {
	v, err := lumeGetVM(VMName(profile))
	if err != nil {
		return false
	}
	return strings.EqualFold(v.Status, "running")
}

// List queries Lume for all VM instances and returns only those managed by
// cloister (identified by the cloister namespace prefix). The raw Lume fields
// are normalised into the shared vm.VMStatus representation. When verbose is
// true, Lume's output is forwarded to stderr for debugging.
func (b *Backend) List(verbose bool) ([]vm.VMStatus, error) {
	var buf bytes.Buffer
	cmd := exec.Command("lume", "ls", "--format", "json")
	if verbose {
		sink := &teeWriter{buf: &buf, w: os.Stderr}
		cmd.Stdout = sink
		cmd.Stderr = sink
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("lume ls: %w\n%s", err, buf.String())
	}

	trimmed := bytes.TrimSpace(buf.Bytes())
	if len(trimmed) == 0 {
		return nil, nil
	}

	var all []lumeVM
	if err := json.Unmarshal(trimmed, &all); err != nil {
		return nil, fmt.Errorf("parsing lume ls output: %w", err)
	}

	// Retain only instances that were created and are managed by cloister.
	var managed []vm.VMStatus
	for _, v := range all {
		if ProfileFromVMName(v.Name) == "" {
			continue
		}
		managed = append(managed, vm.VMStatus{
			Name:   v.Name,
			Status: v.Status,
			CPUs:   v.CPUs,
			Memory: v.memoryGB(),
			Disk:   v.diskGB(),
			Arch:   v.Arch,
		})
	}
	return managed, nil
}

// SSH attaches an interactive terminal session to the VM for the given profile.
// stdin, stdout, and stderr are connected directly to the SSH process so the
// caller receives a fully functional shell.
func (b *Backend) SSH(profile string) error {
	args := b.sshArgs(profile, "")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh to %s: %w", profile, err)
	}
	return nil
}

// SSHCommand runs a non-interactive command inside the VM and returns its
// standard output. Standard error is deliberately not mixed in, so a caller
// parsing the result cannot have it corrupted by a warning; on failure it is
// carried in the error instead, which is the only place a diagnostic written
// there survives. The command is executed directly by the remote shell so
// login-shell initialisation in the guest applies.
func (b *Backend) SSHCommand(profile string, command string) (string, error) {
	args := b.sshArgs(profile, command)
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.Output()
	if err != nil {
		// Include stderr in the error for diagnostics, but return only
		// stdout as the command output so callers can parse it cleanly.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return string(out), fmt.Errorf("ssh command in %s: %w\n%s", profile, err, string(ee.Stderr))
		}
		return string(out), fmt.Errorf("ssh command in %s: %w", profile, err)
	}
	return string(out), nil
}

// SSHInteractive runs a command inside the VM with stdin/stdout/stderr
// connected to the current terminal. This is suitable for streaming commands
// such as log tailing that require direct terminal access.
func (b *Backend) SSHInteractive(profile string, command string) error {
	args := b.sshArgs(profile, command)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SSHScript pipes a multi-line shell script into the VM via stdin. Using stdin
// avoids the shell quoting complications that arise when embedding complex
// scripts as a single command argument.
func (b *Backend) SSHScript(profile string, script string) (string, error) {
	return b.SSHScriptTo(profile, script, nil)
}

// SSHScriptTo is SSHScript with the guest output copied to an additional
// destination. A nil destination keeps the output capture-only; a non-nil
// destination receives the same bytes as they arrive.
//
// The destination is written to while the command runs rather than after it
// exits. --verbose promises the guest output as it happens, and a provisioning
// step that takes minutes looks like a hang until the first byte appears.
func (b *Backend) SSHScriptTo(profile string, script string, out io.Writer) (string, error) {
	args := b.sshArgs(profile, "bash -ls")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader([]byte(script))

	// Every call returns the capture. A sinkless caller also needs it embedded
	// in the error because no other diagnostic destination exists.
	directed := out != nil
	var captured bytes.Buffer
	var sink io.Writer = &captured
	if directed {
		sink = io.MultiWriter(&captured, out)
	}
	// One writer for both streams prevents concurrent writes to the capture
	// buffer. SSH buffers the streams separately, so guest write order is lost.
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Run(); err != nil {
		wrapped := fmt.Errorf("ssh script in %s: %w", profile, err)
		if !directed && captured.Len() > 0 {
			wrapped = fmt.Errorf("%w\nOutput: %s", wrapped, captured.String())
		}
		return captured.String(), wrapped
	}
	return captured.String(), nil
}

// SSHCapture is identical to SSHScript for the lume backend, which already
// captures output rather than streaming it. It exists to satisfy the Backend
// interface's capture-only contract used by control and value-resolution calls.
func (b *Backend) SSHCapture(profile string, script string) (string, error) {
	return b.SSHScript(profile, script)
}

// SSHConfig returns the SSH connection parameters for the given profile. The
// returned SSHAccess values are suitable for constructing an ssh(1) invocation
// or a programmatic SSH client connection by the tunnel manager.
func (b *Backend) SSHConfig(profile string) vm.SSHAccess {
	return vm.SSHAccess{
		Host:    MDNSName(profile),
		User:    "lume",
		KeyFile: KeyPath(profile),
	}
}

// VMName returns the Lume VM name for the given cloister profile by delegating
// to the package-level VMName helper in naming.go.
func (b *Backend) VMName(profile string) string {
	return VMName(profile)
}

// ProfileFromVMName extracts the cloister profile name from a Lume VM name by
// delegating to the package-level ProfileFromVMName helper in naming.go. It
// returns an empty string when the name was not created by cloister.
func (b *Backend) ProfileFromVMName(vmName string) string {
	return ProfileFromVMName(vmName)
}

// VMIP implements vm.NATNetworker by returning the current NAT IP address
// assigned to the VM for the given profile. It parses the JSON output of
// `lume get` and returns an error if the VM is not running or has no IP.
func (b *Backend) VMIP(profile string) (string, error) {
	v, err := lumeGetVM(VMName(profile))
	if err != nil {
		return "", fmt.Errorf("querying IP for %s: %w", profile, err)
	}
	if v.IP == "" {
		return "", fmt.Errorf("no IP address available for %s (VM may not be running)", profile)
	}
	return v.IP, nil
}

// resolveSSHHost determines the best SSH target for the given profile.
// It tries two sources in order, with bounded timeouts on each:
//  1. NAT IP via `lume get` — fast, always correct when VM is running
//  2. mDNS hostname — requires prior hostname configuration via SetHostname
//
// Returns the resolved host and, in verbose mode, logs which source was used.
// If neither source produces a reachable address, it returns an mDNS name;
// sshArgs bounds the connection attempt, though DNS resolution itself may
// still outlive that timeout.
func (b *Backend) resolveSSHHost(profile string) string {
	vmName := VMName(profile)

	v, err := lumeGetVM(vmName)
	if err == nil && v.IP != "" {
		b.debugf("SSH target for %s: using IP %s\n", profile, v.IP)
		return v.IP
	}

	mdns := MDNSName(profile)
	ip := resolveMDNSWithTimeout(mdns, 5*time.Second)
	if ip != "" {
		b.debugf("SSH target for %s: using mDNS %s → %s\n", profile, mdns, ip)
		return ip
	}

	if err == nil && v.IP == "" {
		b.debugf("SSH target for %s: no IP from lume get, mDNS %s did not resolve\n", profile, mdns)
	} else {
		b.debugf("SSH target for %s: lume get failed (%v), mDNS %s did not resolve\n", profile, err, mdns)
	}

	// Return the mDNS name as last resort — sshArgs sets ConnectTimeout
	// so it won't hang forever, but DNS resolution itself may still block.
	// This path should only be reached during initial provisioning before
	// the hostname is set.
	return mdns
}

func (b *Backend) debugf(format string, args ...any) {
	if b.verbose {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// resolveMDNSWithTimeout attempts mDNS resolution with a hard deadline.
// macOS mDNS resolution for non-existent .local hostnames can block for
// 30+ seconds. This runs the lookup in a goroutine and returns empty
// string if the deadline expires.
func resolveMDNSWithTimeout(hostname string, timeout time.Duration) string {
	type result struct{ ip string }
	ch := make(chan result, 1)
	go func() {
		ch <- result{resolveMDNS(hostname)}
	}()
	select {
	case r := <-ch:
		return r.ip
	case <-time.After(timeout):
		return ""
	}
}

func (b *Backend) sshArgs(profile string, command string) []string {
	host := b.resolveSSHHost(profile)
	args := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-i", KeyPath(profile),
		fmt.Sprintf("lume@%s", host),
	}
	if command != "" {
		args = append(args, command)
	}
	return args
}

// lumeGetVM executes `lume get <name> --format json` and returns the parsed
// VM struct. Lume wraps the output in a JSON array even for a single VM, so
// this function unwraps the first element. Returns an error if the VM does
// not exist or the output cannot be parsed.
func lumeGetVM(name string) (*lumeVM, error) {
	cmd := exec.Command("lume", "get", name, "--format", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("lume get %s: %w\n%s", name, err, string(out))
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("lume get %s: empty output", name)
	}

	// Lume always returns an array, even for a single VM.
	var vms []lumeVM
	if err := json.Unmarshal(trimmed, &vms); err != nil {
		return nil, fmt.Errorf("parsing lume get output for %s: %w", name, err)
	}
	if len(vms) == 0 {
		return nil, fmt.Errorf("lume get %s: no VM in output", name)
	}
	return &vms[0], nil
}

// runLume executes `lume <args...>`. When verbose is true, Lume's output is
// streamed to os.Stdout so users can observe progress in real time (IPSW
// download percentages, macOS install progress, unattended setup steps).
// Output is also captured in a buffer for error reporting on failure.
func runLume(verbose bool, args ...string) error {
	cmd := exec.Command("lume", args...)
	var buf bytes.Buffer
	if verbose {
		sink := &teeWriter{buf: &buf, w: os.Stdout}
		cmd.Stdout = sink
		cmd.Stderr = sink
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, buf.String())
	}
	return nil
}

// teeWriter multiplexes writes to both a bytes.Buffer and an io.Writer. Each
// command assigns one teeWriter instance to both stdout and stderr, which makes
// os/exec combine the streams into one ordered pipe rather than call Write
// concurrently. If the destination implements Sync (for example *os.File),
// each write is flushed so long-running setup produces real-time output.
type teeWriter struct {
	buf *bytes.Buffer
	w   interface{ Write([]byte) (int, error) }
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.buf.Write(p)
	n, err := t.w.Write(p)
	if f, ok := t.w.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	return n, err
}
