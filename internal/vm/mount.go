// Package vm provides a subprocess wrapper around the Colima CLI for managing
// the lifecycle of cloister-owned virtual machines. These functions are
// internal implementation details and are not exposed to end users.
package vm

import "path/filepath"

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

// BuildMounts returns the standard set of host directory mounts for a cloister
// VM. The caller supplies the user's home directory so that the function
// remains testable without relying on the process environment.
//
// Mount policy:
//   - ~/Code and the Claude extension directories are read-write so the VM can
//     create, modify, and delete files in those trees.
//   - ~/.ssh, ~/.gnupg, ~/Downloads are read-only to prevent accidental
//     modification of sensitive credential material from inside the VM.
func BuildMounts(homeDir string) []Mount {
	rw := true
	ro := false

	return []Mount{
		// Primary workspace: full read-write access so that code can be edited
		// and built from within the VM.
		{Location: filepath.Join(homeDir, "Code"), Writable: rw},

		// SSH keys: read-only so the VM can authenticate to remote hosts without
		// being able to alter or exfiltrate the private key material.
		{Location: filepath.Join(homeDir, ".ssh"), Writable: ro},

		// GPG keyring: read-only to allow commit signing while preventing
		// modification of the host's cryptographic identity.
		{Location: filepath.Join(homeDir, ".gnupg"), Writable: ro},

		// Downloads: read-only so the VM can access downloaded artefacts without
		// being able to write back to the host's Downloads folder.
		{Location: filepath.Join(homeDir, "Downloads"), Writable: ro},

		// Claude extension directories: read-write so that plugin, skill, and
		// agent files can be installed or updated from within the VM.
		{Location: filepath.Join(homeDir, ".claude", "plugins"), Writable: rw},
		{Location: filepath.Join(homeDir, ".claude", "skills"), Writable: rw},
		{Location: filepath.Join(homeDir, ".claude", "agents"), Writable: rw},
	}
}
