package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resource defaults applied when a profile field is not explicitly configured.
const (
	// DefaultMemory is the default VM memory allocation in gigabytes.
	DefaultMemory = 4

	// DefaultDisk is the default VM disk size in gigabytes.
	DefaultDisk = 40

	// DefaultCPU is the default number of virtual CPUs assigned to a VM.
	DefaultCPU = 4

	// DefaultStartDir is the default working directory inside the VM.
	DefaultStartDir = "~/code"

	// MinBudget is the minimum allowable memory budget in gigabytes,
	// ensuring the VM always has enough memory to operate.
	MinBudget = 4

	// ReservedForHost is the amount of RAM in gigabytes that must remain
	// available for the host operating system before any VM budget is allocated.
	ReservedForHost = 10
)

// CalculateBudget returns the maximum VM memory budget in gigabytes given the
// host's total RAM. At least MinBudget is always returned so that low-memory
// hosts can still run a minimal workload.
func CalculateBudget(totalRAMGB int) int {
	budget := totalRAMGB - ReservedForHost
	if budget < MinBudget {
		return MinBudget
	}
	return budget
}

// ResolveWorkspaceDir converts a profile's start_dir value into an absolute
// filesystem path. A leading "~/" is expanded using homeDir; a bare "~" is
// resolved to homeDir itself. When startDir is empty the function returns the
// default workspace path (homeDir/code).
//
// An error is returned for paths that cannot be safely resolved:
//   - Relative paths (no leading "/" or "~") are rejected because Colima
//     requires absolute mount targets.
//   - The "~otheruser" syntax is not supported; only the current user's home
//     directory may be referenced via the tilde shorthand.
func ResolveWorkspaceDir(startDir string, homeDir string) (string, error) {
	if startDir == "" {
		return filepath.Join(homeDir, "code"), nil
	}
	if startDir == "~" {
		return homeDir, nil
	}
	if strings.HasPrefix(startDir, "~/") {
		return filepath.Join(homeDir, startDir[2:]), nil
	}
	// Detect "~otheruser" — a tilde followed by any characters other than "/"
	// is the POSIX ~user expansion syntax, which we intentionally do not
	// support to avoid silently resolving to an unexpected directory.
	if strings.HasPrefix(startDir, "~") {
		return "", fmt.Errorf("workspace directory %q uses ~user syntax which is not supported — use an absolute path", startDir)
	}
	// Reject relative paths; Colima requires absolute paths for mount targets.
	if !filepath.IsAbs(startDir) {
		return "", fmt.Errorf("workspace directory %q is not an absolute path — use an absolute path or ~/relative/path", startDir)
	}
	return startDir, nil
}

// ApplyDefaults fills any zero-value resource fields on the Profile with the
// package-level default constants. Fields that have already been set by the
// user are left unchanged.
func (p *Profile) ApplyDefaults() {
	if p.Memory == 0 {
		p.Memory = DefaultMemory
	}
	if p.Disk == 0 {
		p.Disk = DefaultDisk
	}
	if p.CPU == 0 {
		p.CPU = DefaultCPU
	}
	if p.StartDir == "" {
		p.StartDir = DefaultStartDir
	}
}
