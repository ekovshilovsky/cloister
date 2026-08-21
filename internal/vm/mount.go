// Package vm provides a subprocess wrapper around the Colima CLI for managing
// the lifecycle of cloister-owned virtual machines. These functions are
// internal implementation details and are not exposed to end users.
package vm

import (
	"os"
	"path/filepath"

	"cloister.io/internal/config"
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
// cloister may bind into a VM. Workspace transport is deliberately absent.
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
	//
	// The plugins directory is split into granular subdirectory mounts so that
	// environment-agnostic data (cache, marketplaces) is shared while index
	// files containing absolute host paths are excluded from the VM.
	{name: "claude-plugins-cache", subpath: ".claude/plugins/cache", writable: true},
	{name: "claude-plugins-marketplaces", subpath: ".claude/plugins/marketplaces", writable: true},
	{name: "claude-skills", subpath: ".claude/skills", writable: true},
	{name: "claude-agents", subpath: ".claude/agents", writable: true},

	// Agent skill definitions directory: mounted read-write so that symlinks
	// in ~/.claude/skills/ that reference paths under ~/.agents/ resolve
	// correctly inside the VM.
	{name: "agents-skills", subpath: ".agents", writable: true},
}

// claudeExtensionNames is the set of mount names that are demoted to read-only
// when running under a headless profile. Centralised here to avoid scattering
// the headless restriction logic across the implementation.
var claudeExtensionNames = map[string]bool{
	"claude-plugins-cache":        true,
	"claude-plugins-marketplaces": true,
	"claude-skills":               true,
	"claude-agents":               true,
	"agents-skills":               true,
}

// MountsChanged reports whether mount location, guest destination, writability,
// order, or cardinality changed.
func MountsChanged(before, after []Mount) bool {
	if len(before) != len(after) {
		return true
	}
	for i := range before {
		if before[i] != after[i] {
			return true
		}
	}
	return false
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

// VMHome returns the home directory path inside a Colima Linux VM. Colima
// VMs use the host username (not the profile name) with a ".guest" suffix.
func VMHome(hostHomeDir string) string {
	user := filepath.Base(hostHomeDir)
	return "/home/" + user + ".guest"
}

// BuildSupplementalMounts constructs fixed host resource bindings for a
// cloister VM. Workspace exposure is owned by the lifecycle coordinator and is
// never inferred from this list. Standard directories are filtered by policy
// and headless restrictions. The Ollama model store is appended when active.
//
// Mounts use Colima's default pass-through behavior where the host path is
// used as both the location and mount point inside the VM. VM-side tools
// that need to find these paths use the host home directory (available via
// the cloister-vm config) rather than $HOME.
//
// Parameters:
//   - homeDir:      Absolute path to the user's home directory on the host.
//   - stacks:       Toolchain stacks active for the profile (e.g. ["ollama"]).
//   - mountPolicy:  Consent policy controlling which named mounts are permitted.
//   - isHeadless:   Whether the profile runs without an attached terminal.
func BuildSupplementalMounts(homeDir string, stacks []string, mountPolicy config.ResourcePolicy, isHeadless bool) []Mount {
	var mounts []Mount
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
