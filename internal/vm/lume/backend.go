package lume

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Backend implements vm.Backend and vm.NATNetworker by wrapping the Lume CLI.
// Each method translates a cloister profile name into the corresponding Lume VM
// identifier and delegates to the appropriate `lume` sub-command. SSH
// connectivity is established using per-profile Ed25519 keys managed by keys.go
// and the VM's mDNS hostname in the ".local" domain.
type Backend struct{}

// lumeVM is the JSON structure emitted by `lume get <name> --format json` and
// by individual entries in `lume ls --format json`. Only the fields consumed
// by cloister are declared; additional fields returned by Lume are silently
// ignored.
type lumeVM struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CPUs       int        `json:"cpuCount"`
	MemorySize int64      `json:"memorySize"`
	DiskSize   lumeDisk   `json:"diskSize"`
	Arch       string     `json:"arch"`
	IP         string     `json:"ipAddress"`
	OS         string     `json:"os"`
	Created    time.Time  `json:"created"`
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
// Note: cpus, memoryGB, and diskGB are accepted for interface compliance but
// are not applied at run time. Lume configures resources at creation via
// `lume set`, not at boot via `lume run`. The createLumeProfile flow calls
// `lume set` before Start to apply the desired resources.
func (b *Backend) Start(profile string, cpus, memoryGB, diskGB int, mounts []vm.Mount, verbose bool) error {
	name := VMName(profile)
	args := []string{"run", name, "--no-display"}

	// Lume uses Apple's VirtioFS with a single shared tag
	// (com.apple.virtio-fs.automount). Only one --shared-dir is supported
	// per VM — passing multiple causes a configuration error. Use only the
	// first mount (workspace directory), which is the critical one.
	if len(mounts) > 0 {
		m := mounts[0]
		if m.Writable {
			args = append(args, "--shared-dir", m.Location)
		} else {
			args = append(args, "--shared-dir", fmt.Sprintf("%s:ro", m.Location))
		}
	}

	// lume run is a foreground command that blocks until the VM stops.
	// Start it as a detached background process so cloister can proceed
	// with provisioning while the VM runs.
	cmd := exec.Command("lume", args...)
	if verbose {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lume run %s: %w", name, err)
	}

	// Detach — don't wait for the process to exit.
	go func() { _ = cmd.Wait() }()

	return nil
}

// Stop gracefully shuts down the running VM for the given profile. When verbose
// is true, Lume's output is forwarded to stderr.
func (b *Backend) Stop(profile string, verbose bool) error {
	name := VMName(profile)
	if err := runLume(verbose, "stop", name); err != nil {
		return fmt.Errorf("lume stop %s: %w", name, err)
	}
	return nil
}

// Delete permanently destroys the VM for the given profile and releases all
// associated resources. The --force flag allows deletion regardless of the
// current VM state. When verbose is true, Lume's output is forwarded to stderr.
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
		cmd.Stdout = &teeWriter{buf: &buf, w: os.Stderr}
		cmd.Stderr = &teeWriter{buf: &buf, w: os.Stderr}
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
	args := sshArgs(profile, "")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh to %s: %w", profile, err)
	}
	return nil
}

// SSHCommand runs a non-interactive command inside the VM and returns the
// combined stdout and stderr output. The command is executed directly by the
// remote shell so login-shell initialisation in the guest applies.
func (b *Backend) SSHCommand(profile string, command string) (string, error) {
	args := sshArgs(profile, command)
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh command in %s: %w", profile, err)
	}
	return string(out), nil
}

// SSHInteractive runs a command inside the VM with stdin/stdout/stderr
// connected to the current terminal. This is suitable for streaming commands
// such as log tailing that require direct terminal access.
func (b *Backend) SSHInteractive(profile string, command string) error {
	args := sshArgs(profile, command)
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
	args := sshArgs(profile, "bash -ls")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader([]byte(script))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh script in %s: %w\nOutput: %s", profile, err, string(out))
	}
	return string(out), nil
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

// resolveSSHHost determines the best SSH target for the given profile. It
// queries `lume get` for the VM's current NAT IP address first — this is
// always correct and has no DNS resolution delay. If the IP is unavailable
// (VM not fully booted yet), it falls back to the mDNS name which works
// once the hostname has been configured.
//
// This two-step resolution avoids the 10-second ConnectTimeout penalty that
// occurs when mDNS is used before the hostname is set (during provisioning),
// while still supporting mDNS-based access for normal steady-state operations.
func resolveSSHHost(profile string) string {
	v, err := lumeGetVM(VMName(profile))
	if err == nil && v.IP != "" {
		return v.IP
	}
	return MDNSName(profile)
}

// sshArgs constructs the ssh(1) argument slice for connecting to the VM for
// the given profile. The SSH target host is resolved dynamically via
// resolveSSHHost, which prefers the VM's NAT IP (fast, always works) over
// the mDNS hostname (requires prior hostname configuration).
//
// When command is non-empty it is appended as the remote command to execute;
// when empty the connection opens an interactive shell. StrictHostKeyChecking
// is disabled because Lume VMs are ephemeral and their host keys change
// across provisioning cycles.
func sshArgs(profile string, command string) []string {
	host := resolveSSHHost(profile)
	args := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
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
		// Tee to both stdout (user-visible) and buffer (for error reporting).
		cmd.Stdout = &teeWriter{buf: &buf, w: os.Stdout}
		cmd.Stderr = &teeWriter{buf: &buf, w: os.Stdout}
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, buf.String())
	}
	return nil
}

// teeWriter multiplexes writes to both a bytes.Buffer and an io.Writer,
// allowing output to be captured for error reporting while simultaneously
// being streamed to the terminal for live observability.
type teeWriter struct {
	buf *bytes.Buffer
	w   interface{ Write([]byte) (int, error) }
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.buf.Write(p)
	return t.w.Write(p)
}
