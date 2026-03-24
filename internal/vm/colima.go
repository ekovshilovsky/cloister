package vm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// vmPrefix is prepended to every profile name to form the Colima instance
// name. This namespace prevents cloister-managed VMs from colliding with
// any Colima instances the user may have created independently.
const vmPrefix = "cloister-"

// VMName returns the Colima instance name for the given cloister profile.
// All Colima operations performed by cloister use this name so that managed
// VMs are clearly identified and easily filtered.
//
// Deprecated: new code should use Backend.VMName instead. This function is
// retained only because the backward-compat wrappers in agent/manager.go
// still reference it.
func VMName(profile string) string {
	return vmPrefix + profile
}

// ProfileFromVMName extracts the cloister profile name from a Colima instance
// name. It returns an empty string when the instance name does not carry the
// cloister prefix, which indicates the VM was not created by cloister.
//
// Deprecated: new code should use Backend.ProfileFromVMName instead. This
// function is retained only because the backward-compat wrappers in
// agent/manager.go still reference it indirectly through the naming helpers.
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

// SSHCommand runs a non-interactive shell command inside the VM and returns the
// combined stdout and stderr output. The command is executed via `bash -lc` so
// that login-shell initialisation (PATH, environment variables) is performed
// before the command runs.
//
// Deprecated: new code should use Backend.SSHCommand instead. This function is
// retained only because the backward-compat wrappers in agent/manager.go
// still call it.
func SSHCommand(profile string, command string) (string, error) {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-lc", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("colima ssh command in %s: %w", name, err)
	}
	return string(out), nil
}

// SSHInteractive runs a command inside the VM with stdin/stdout/stderr
// connected to the current terminal. Used for streaming commands like
// docker logs -f that need direct terminal access.
//
// Deprecated: new code should use Backend.SSHInteractive instead. This function
// is retained only because the backward-compat wrappers in agent/manager.go
// still call it.
func SSHInteractive(profile string, command string) error {
	name := VMName(profile)
	cmd := exec.Command("colima", "ssh", "--profile", name, "--", "bash", "-lc", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
