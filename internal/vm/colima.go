package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// vmPrefix is prepended to every profile name to form the Colima instance
// name. This namespace prevents cloister-managed VMs from colliding with
// any Colima instances the user may have created independently.
const vmPrefix = "cloister-"

// VMName returns the Colima instance name for the given cloister profile.
// All Colima operations performed by cloister use this name so that managed
// VMs are clearly identified and easily filtered.
func VMName(profile string) string {
	return vmPrefix + profile
}

// ProfileFromVMName extracts the cloister profile name from a Colima instance
// name. It returns an empty string when the instance name does not carry the
// cloister prefix, which indicates the VM was not created by cloister.
func ProfileFromVMName(vmName string) string {
	if !strings.HasPrefix(vmName, vmPrefix) {
		return ""
	}
	profile := strings.TrimPrefix(vmName, vmPrefix)
	// Reject the degenerate case where the name equals the bare prefix with no
	// profile segment following it (e.g. "cloister-").
	if profile == "" {
		return ""
	}
	return profile
}

// VMStatus captures the subset of Colima instance metadata that cloister
// needs for display and lifecycle decisions. The JSON tags match the field
// names emitted by `colima list --json`.
type VMStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	CPUs   int    `json:"cpus"`
	Memory int    `json:"memory"`
	Disk   int    `json:"disk"`
	Arch   string `json:"arch"`
}

// List queries Colima for all running and stopped instances and returns only
// those that were created by cloister (identified by vmPrefix). When verbose
// is true, the raw Colima output is forwarded to stderr for debugging.
func List(verbose bool) ([]VMStatus, error) {
	out, err := runColima(verbose, "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("colima list: %w", err)
	}

	var all []VMStatus

	// Colima may emit either a JSON array or newline-delimited JSON objects
	// depending on the version. Handle both formats.
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}

	if trimmed[0] == '[' {
		// Full JSON array.
		if err := json.Unmarshal(trimmed, &all); err != nil {
			return nil, fmt.Errorf("parsing colima list output: %w", err)
		}
	} else {
		// Newline-delimited JSON objects (one per line).
		for _, line := range strings.Split(string(trimmed), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var s VMStatus
			if err := json.Unmarshal([]byte(line), &s); err != nil {
				return nil, fmt.Errorf("parsing colima list line %q: %w", line, err)
			}
			all = append(all, s)
		}
	}

	// Retain only instances managed by cloister.
	var managed []VMStatus
	for _, s := range all {
		if ProfileFromVMName(s.Name) != "" {
			managed = append(managed, s)
		}
	}
	return managed, nil
}

// IsRunning reports whether the VM for the given profile is currently in the
// running state. It returns false if Colima reports an error or if the
// instance does not exist.
func IsRunning(profile string) bool {
	statuses, err := List(false)
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

// Start creates or starts the Colima VM for the given profile with the
// specified resource allocation. The VM is configured to use the Virtualization
// Framework (vz) hypervisor with virtiofs for low-latency mount I/O, targeting
// the Apple Silicon native architecture. Each entry in mounts is appended as a
// --mount flag. When verbose is true, Colima's output is forwarded to stderr.
func Start(profile string, cpus, memoryGB, diskGB int, mounts []Mount, verbose bool) error {
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

// Stop gracefully stops the running VM for the given profile. It is a no-op
// when the VM is already stopped. When verbose is true, Colima's output is
// forwarded to stderr.
func Stop(profile string, verbose bool) error {
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
func Delete(profile string, verbose bool) error {
	name := VMName(profile)
	_, err := runColima(verbose, "delete", "--profile", name, "--force")
	if err != nil {
		return fmt.Errorf("colima delete %s: %w", name, err)
	}
	return nil
}

// SSH attaches an interactive terminal session to the VM for the given profile.
// stdin, stdout, and stderr are connected directly to the process so the caller
// receives a fully functional shell.
func SSH(profile string) error {
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

// SSHCommand runs a non-interactive shell command inside the VM and returns the
// combined stdout and stderr output. The command is executed via `bash -lc` so
// that login-shell initialisation (PATH, environment variables) is performed
// before the command runs.
func SSHCommand(profile string, command string) (string, error) {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-lc", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("colima ssh command in %s: %w", name, err)
	}
	return string(out), nil
}

// SSHScript pipes a multi-line script to bash inside the VM via stdin.
// This avoids shell quoting issues that occur when passing complex scripts
// as a single bash -c argument.
func SSHScript(profile string, script string) (string, error) {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-ls")
	cmd.Stdin = bytes.NewReader([]byte(script))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("colima ssh script in %s: %w\nOutput: %s", name, err, string(out))
	}
	return string(out), nil
}

// SSHConfig returns the path to the SSH client configuration file that Lima
// (the hypervisor layer beneath Colima) generates for the given profile. This
// file can be passed to `ssh -F` to reach the VM without additional parameters.
// SSHConfig returns the path to the VM's SSH config file.
// Colima prefixes its own "colima-" to the profile name in the Lima directory
// structure, so the path is ~/.colima/_lima/colima-<vmName>/ssh.config.
func SSHConfig(profile string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".colima", "_lima", "colima-"+VMName(profile), "ssh.config")
}

// SSHHost returns the Lima SSH host name used in the SSH config file.
// Format: lima-colima-<vmName>
func SSHHost(profile string) string {
	return "lima-colima-" + VMName(profile)
}

// Exists reports whether a Colima instance for the given profile is present
// in either the running or stopped state. It uses `colima status` so that the
// check is lightweight and does not parse the full instance list.
func Exists(profile string) bool {
	name := VMName(profile)
	cmd := exec.Command("colima", "status", "--profile", name)
	return cmd.Run() == nil
}

// mountFlag converts a Mount into the argument string expected by
// `colima start --mount`. Colima's mount flag syntax is:
//
//	<location>[:<mountpoint>][:w]
//
// When MountPoint is empty the VM uses the same path as the host (Colima
// default). When Writable is true the ":w" suffix grants write access.
func mountFlag(m Mount) string {
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

// runColima executes `colima <args...>` and returns the combined output. When
// verbose is true the output is also forwarded to os.Stderr so the caller can
// observe progress in real time.
func runColima(verbose bool, args ...string) ([]byte, error) {
	cmd := exec.Command("colima", args...)
	var buf bytes.Buffer
	if verbose {
		// Tee output to both the buffer and stderr so the caller sees live
		// progress while we also capture it for programmatic consumption.
		cmd.Stdout = &teeWriter{buf: &buf, w: os.Stderr}
		cmd.Stderr = &teeWriter{buf: &buf, w: os.Stderr}
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	err := cmd.Run()
	return buf.Bytes(), err
}

// colimaArch returns the Colima architecture flag for the current host.
// Maps Go's runtime.GOARCH to Colima's architecture naming.
func colimaArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// teeWriter multiplexes writes to both a bytes.Buffer and an io.Writer,
// allowing output to be captured for processing while simultaneously being
// streamed to the terminal.
type teeWriter struct {
	buf *bytes.Buffer
	w   interface{ Write([]byte) (int, error) }
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.buf.Write(p)
	return t.w.Write(p)
}
