package colima

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// Backend implements vm.Backend by wrapping the Colima CLI. Each method
// translates a cloister profile name into the corresponding Colima instance
// identifier and delegates to the appropriate `colima` sub-command.
type Backend struct{}

// Start creates or resumes the Colima VM for the given profile with the
// specified resource allocation. The VM is configured to use the Virtualization
// Framework (vz) hypervisor with virtiofs for low-latency mount I/O, targeting
// the host's native CPU architecture. Each entry in mounts is appended as a
// --mount flag. When verbose is true, Colima's output is forwarded to stderr.
func (b *Backend) Start(profile string, cpus, memoryGB, diskGB int, mounts []vm.Mount, verbose bool) error {
	name := VMName(profile)

	args := []string{
		"start",
		"--profile", name,
		"--cpu", fmt.Sprintf("%d", cpus),
		"--memory", fmt.Sprintf("%d", memoryGB),
		"--disk", fmt.Sprintf("%d", diskGB),
		"--vm-type", "vz",
		"--mount-type", "virtiofs",
		"--arch", colimaArch(),
	}

	for _, m := range mounts {
		args = append(args, "--mount", mountFlag(m))
	}

	_, err := runColima(verbose, args...)
	if err != nil {
		return fmt.Errorf("colima start %s: %w", name, err)
	}
	return nil
}

// Stop gracefully shuts down the running VM for the given profile. It is
// idempotent: stopping an already-stopped VM will not return an error from
// Colima. When verbose is true, Colima's output is forwarded to stderr.
func (b *Backend) Stop(profile string, verbose bool) error {
	name := VMName(profile)
	_, err := runColima(verbose, "stop", "--profile", name)
	if err != nil {
		return fmt.Errorf("colima stop %s: %w", name, err)
	}
	return nil
}

// Delete permanently destroys the VM for the given profile, releasing all
// disk and memory resources. The --force flag allows deletion even when the
// VM is currently running. When verbose is true, Colima's output is forwarded
// to stderr.
func (b *Backend) Delete(profile string, verbose bool) error {
	name := VMName(profile)
	_, err := runColima(verbose, "delete", "--profile", name, "--force")
	if err != nil {
		return fmt.Errorf("colima delete %s: %w", name, err)
	}
	return nil
}

// Exists reports whether a Colima instance for the given profile is present
// in either the running or stopped state. It uses `colima status` so that the
// check is lightweight and does not parse the full instance list.
func (b *Backend) Exists(profile string) bool {
	name := VMName(profile)
	cmd := exec.Command("colima", "status", "--profile", name)
	return cmd.Run() == nil
}

// IsRunning reports whether the VM for the given profile is currently in the
// running state. It returns false when Colima reports an error or when the
// instance does not exist.
func (b *Backend) IsRunning(profile string) bool {
	statuses, err := b.List(false)
	if err != nil {
		return false
	}
	name := VMName(profile)
	for _, s := range statuses {
		if s.Name == name {
			return strings.EqualFold(s.Status, "running")
		}
	}
	return false
}

// List queries Colima for all instances and returns only those managed by
// cloister (identified by the cloister namespace prefix). When verbose is
// true, the raw Colima output is forwarded to stderr for debugging.
func (b *Backend) List(verbose bool) ([]vm.VMStatus, error) {
	out, err := runColima(verbose, "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("colima list: %w", err)
	}

	var all []vm.VMStatus

	// Colima may emit either a JSON array or newline-delimited JSON objects
	// depending on the version. Handle both formats.
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}

	if trimmed[0] == '[' {
		// Full JSON array format.
		if err := json.Unmarshal(trimmed, &all); err != nil {
			return nil, fmt.Errorf("parsing colima list output: %w", err)
		}
	} else {
		// Newline-delimited JSON objects (one object per line).
		for _, line := range strings.Split(string(trimmed), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var s vm.VMStatus
			if err := json.Unmarshal([]byte(line), &s); err != nil {
				return nil, fmt.Errorf("parsing colima list line %q: %w", line, err)
			}
			all = append(all, s)
		}
	}

	// Retain only instances that were created and are managed by cloister.
	var managed []vm.VMStatus
	for _, s := range all {
		if ProfileFromVMName(s.Name) != "" {
			managed = append(managed, s)
		}
	}
	return managed, nil
}

// SSH attaches an interactive terminal session to the VM for the given profile.
// stdin, stdout, and stderr are connected directly to the Colima process so
// the caller receives a fully functional shell.
func (b *Backend) SSH(profile string) error {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("colima ssh %s: %w", name, err)
	}
	return nil
}

// SSHCommand runs a non-interactive command inside the VM and returns the
// combined stdout and stderr output. The command is executed via `bash -lc`
// so that login-shell initialisation (PATH, environment variables) is
// performed before the command runs.
func (b *Backend) SSHCommand(profile string, command string) (string, error) {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-lc", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("colima ssh command in %s: %w", name, err)
	}
	return string(out), nil
}

// SSHInteractive runs a command inside the VM with stdin/stdout/stderr
// connected to the current terminal. This is suitable for streaming commands
// such as `docker logs -f` that require direct terminal access.
func (b *Backend) SSHInteractive(profile string, command string) error {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-lc", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SSHScript pipes a multi-line shell script into the VM via stdin. This avoids
// the shell quoting problems that arise when embedding complex scripts as a
// single bash -c argument.
func (b *Backend) SSHScript(profile string, script string) (string, error) {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-ls")
	cmd.Stdin = bytes.NewReader([]byte(script))

	// Stream stdout and stderr to the terminal in real-time while also
	// capturing the output for error reporting. This provides live progress
	// visibility during provisioning instead of buffering until completion.
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)

	err := cmd.Run()
	if err != nil {
		return buf.String(), fmt.Errorf("colima ssh script in %s: %w\nOutput: %s", name, err, buf.String())
	}
	return buf.String(), nil
}

// SSHConfig returns the SSH connection parameters for the given profile. The
// ConfigFile field holds the Lima-generated ssh.config path and the HostAlias
// field holds the corresponding Host entry within that file.
func (b *Backend) SSHConfig(profile string) vm.SSHAccess {
	return vm.SSHAccess{
		ConfigFile: SSHConfigPath(profile),
		HostAlias:  SSHHost(profile),
	}
}

// VMName returns the Colima instance name for the given cloister profile by
// delegating to the package-level VMName helper in naming.go.
func (b *Backend) VMName(profile string) string {
	return VMName(profile)
}

// ProfileFromVMName extracts the cloister profile name from a Colima instance
// name by delegating to the package-level ProfileFromVMName helper in naming.go.
// It returns an empty string when the name was not created by cloister.
func (b *Backend) ProfileFromVMName(vmName string) string {
	return ProfileFromVMName(vmName)
}

// runColima executes `colima <args...>` and returns the combined stdout and
// stderr output. When verbose is true the output is also forwarded to os.Stderr
// so the caller can observe Colima's progress in real time.
func runColima(verbose bool, args ...string) ([]byte, error) {
	cmd := exec.Command("colima", args...)
	var buf bytes.Buffer
	if verbose {
		// Tee output to both the capture buffer and stderr so the caller sees
		// live progress while we also retain it for programmatic consumption.
		cmd.Stdout = &teeWriter{buf: &buf, w: os.Stderr}
		cmd.Stderr = &teeWriter{buf: &buf, w: os.Stderr}
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	err := cmd.Run()
	return buf.Bytes(), err
}

// colimaArch returns the Colima architecture flag appropriate for the current
// host. It maps Go's runtime.GOARCH values to Colima's architecture naming
// convention.
func colimaArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// mountFlag converts a vm.Mount into the argument string expected by the
// `colima start --mount` flag. Colima's mount flag syntax is:
//
//	<location>[:<mountpoint>][:w]
//
// When MountPoint is empty the VM uses the same path as Location (Colima
// default). When Writable is true the ":w" suffix grants write access inside
// the guest.
func mountFlag(m vm.Mount) string {
	var sb strings.Builder
	sb.WriteString(m.Location)
	if m.MountPoint != "" {
		sb.WriteByte(':')
		sb.WriteString(m.MountPoint)
	}
	if m.Writable {
		sb.WriteString(":w")
	}
	return sb.String()
}

// teeWriter multiplexes writes to both a bytes.Buffer and an io.Writer,
// allowing output to be captured for processing while simultaneously being
// streamed to the terminal for live observability.
type teeWriter struct {
	buf *bytes.Buffer
	w   interface{ Write([]byte) (int, error) }
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.buf.Write(p)
	return t.w.Write(p)
}
