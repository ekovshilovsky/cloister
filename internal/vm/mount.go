// Package vm provides a subprocess wrapper around the Colima CLI for managing
// the lifecycle of cloister-owned virtual machines. These functions are
// internal implementation details and are not exposed to end users.
package vm

import (
	"os"
	"path/filepath"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// Mount describes a single host-to-VM directory binding passed to Colima at
// VM creation time.
type Mount struct {
	// Location is the absolute path on the macOS host.
	Location string

	// MountPoint is the absolute path inside the VM. When empty, Colima uses
	// the same path as Location (pass-through mounting).
	MountPoint string

	// Writable controls whether the guest may write to the mounted directory.
	// Read-only mounts expose host data to the VM without granting write access.
	Writable bool
}

// mountDef describes a named, configurable host-to-VM directory binding. Each
// entry is evaluated against the active mount policy; only allowed names are
// included in the final mount list.
type mountDef struct {
	// name is the policy key used in allowlist evaluation (e.g. "code", "ssh").
	name string

	// subpath is the path segment relative to the user's home directory.
	subpath string

	// writable is the default writability for interactive profiles. Headless
	// profiles may override this to read-only for sensitive directories.
	writable bool
}

// standardMounts is the ordered catalog of supplemental host directories that
// cloister may bind into a VM. The workspace mount is always prepended
// separately (with a caller-supplied path) and is not listed here. Each entry
// is filtered by the active mount policy before being included in the final
// mount list.
var standardMounts = []mountDef{
	// SSH keys: read-only so the VM can authenticate to remote hosts without
	// being able to alter or exfiltrate the private key material.
	{name: "ssh", subpath: ".ssh", writable: false},

	// GPG keyring: read-only to allow commit signing while preventing
	// modification of the host's cryptographic identity.
	{name: "gnupg", subpath: ".gnupg", writable: false},

	// Downloads: read-only so the VM can access downloaded artefacts without
	// being able to write back to the host's Downloads folder.
	{name: "downloads", subpath: "Downloads", writable: false},

	// Claude extension directories: read-write so that plugin, skill, and
	// agent files can be installed or updated from within an interactive VM.
	// Headless profiles receive these as read-only to prevent unattended
	// modification of host extension material.
	{name: "claude-plugins", subpath: ".claude/plugins", writable: true},
	{name: "claude-skills", subpath: ".claude/skills", writable: true},
	{name: "claude-agents", subpath: ".claude/agents", writable: true},
}

// claudeExtensionNames is the set of mount names that are demoted to read-only
// when running under a headless profile. Centralised here to avoid scattering
// the headless restriction logic across the implementation.
var claudeExtensionNames = map[string]bool{
	"claude-plugins": true,
	"claude-skills":  true,
	"claude-agents":  true,
}

// MountsChanged reports whether two mount lists differ in length, indicating
// that a new mount was added or an existing one was removed between evaluations.
// BuildMounts only appends entries, so a length difference is sufficient to
// detect any change in the set of host-to-VM directory bindings.
func MountsChanged(before, after []Mount) bool {
	return len(before) != len(after)
}

// hasStack reports whether the named stack is present in the stacks slice.
func hasStack(stacks []string, name string) bool {
	for _, s := range stacks {
		if s == name {
			return true
		}
	}
	return false
}

// BuildMounts constructs the set of host-to-VM directory bindings for a
// cloister VM. The workspace directory is always prepended as the first,
// writable mount regardless of the mount policy. Additional standard directories
// (SSH keys, GPG, Downloads, Claude extensions) are then filtered by the
// supplied policy and headless restrictions. The Ollama model store is appended
// when the ollama stack is active and the directory exists on disk.
//
// Parameters:
//   - homeDir:      Absolute path to the user's home directory on the host.
//   - workspaceDir: Absolute path to the workspace directory to mount (derived
//     from the profile's start_dir field via config.ResolveWorkspaceDir).
//   - stacks:       Toolchain stacks active for the profile (e.g. ["ollama"]).
//   - mountPolicy:  Consent policy controlling which named mounts are permitted.
//   - isHeadless:   Whether the profile runs without an attached terminal.
func BuildMounts(homeDir string, workspaceDir string, stacks []string, mountPolicy config.ResourcePolicy, isHeadless bool) []Mount {
	// The workspace mount is unconditionally prepended as the first entry so
	// that the VM always has read-write access to the user's project directory
	// regardless of any mount policy restrictions.
	mounts := []Mount{{
		Location: workspaceDir,
		Writable: true,
	}}

	// Resolve environment-aware defaults when the supplemental policy is unset.
	resolved := mountPolicy.ResolveForMounts(isHeadless)

	for _, def := range standardMounts {
		if !resolved.IsAllowed(def.name) {
			continue
		}

		writable := def.writable
		// Headless profiles receive Claude extension directories as read-only
		// to prevent unattended mutation of host extension material.
		if isHeadless && claudeExtensionNames[def.name] {
			writable = false
		}

		mounts = append(mounts, Mount{
			Location: filepath.Join(homeDir, def.subpath),
			Writable: writable,
		})
	}

	// Append the Ollama model store when the ollama stack is active and the
	// directory is present on the host. The check avoids mounting a path that
	// does not yet exist, which would cause Colima to reject the configuration.
	if hasStack(stacks, "ollama") {
		ollamaModels := filepath.Join(homeDir, ".ollama", "models")
		if _, err := os.Stat(ollamaModels); err == nil {
			mounts = append(mounts, Mount{
				Location: ollamaModels,
				Writable: false,
			})
		}
	}

	return mounts
}
