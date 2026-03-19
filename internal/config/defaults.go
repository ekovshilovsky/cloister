package config

import (
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
// filesystem path. A leading tilde is expanded using homeDir; a bare tilde is
// resolved to homeDir itself. When startDir is empty the function returns the
// default workspace path (homeDir/code).
func ResolveWorkspaceDir(startDir string, homeDir string) string {
	if startDir == "" {
		return filepath.Join(homeDir, "code")
	}
	if startDir == "~" {
		return homeDir
	}
	if strings.HasPrefix(startDir, "~/") {
		return filepath.Join(homeDir, startDir[2:])
	}
	return startDir
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
