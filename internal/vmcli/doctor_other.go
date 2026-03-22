//go:build !linux

package vmcli

// platformChecks returns an empty slice on non-Linux platforms. The system-level
// diagnostics (swap, disk, memory) depend on Linux-specific APIs (/proc filesystem,
// syscall.Statfs behavior) and are not applicable during macOS development builds.
func platformChecks() []struct {
	name string
	fn   func() CheckResult
} {
	return nil
}
