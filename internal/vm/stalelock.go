package vm

// StaleLockDiagnosis describes why a VM failed to start because a previous,
// uncleanly-terminated VM process is still holding the instance disk locked.
// It is produced by backends that implement StaleLockRecoverer and is intended
// for display to the user before any recovery is attempted.
type StaleLockDiagnosis struct {
	// VMName is the hypervisor-level instance name affected by the stale lock.
	VMName string

	// DiskPath is the absolute path to the disk image that remains locked.
	DiskPath string

	// OrphanPIDs are the process IDs proven to still hold DiskPath open. These
	// are the processes recovery would terminate. The slice may be empty when
	// only stale state files (PID/socket) remain and no live process is holding
	// the disk.
	OrphanPIDs []int

	// Summary is a human-readable explanation of the situation, suitable for
	// printing directly to the user.
	Summary string
}

// StaleLockRecoverer is an optional capability for backends whose hypervisor
// can leave a disk locked by an orphaned VM process after a host crash (e.g.
// Colima/Lima on Apple Virtualization.framework). Backends that cannot exhibit
// this failure mode simply do not implement it, and callers fall back to
// surfacing the original start error unchanged.
//
// Callers obtain an implementation via a type assertion on a vm.Backend:
//
//	if rec, ok := backend.(vm.StaleLockRecoverer); ok { ... }
type StaleLockRecoverer interface {
	// DiagnoseStartFailure inspects backend state after a failed Start for the
	// given profile and returns a diagnosis when the failure is attributable to
	// a stale disk lock. It returns nil for any other failure, so unrelated
	// start errors are not misclassified. The check must be conservative: if a
	// live manager process for the instance exists (i.e. the VM is genuinely
	// running), it must return nil rather than offer to kill a healthy VM.
	DiagnoseStartFailure(profile string) *StaleLockDiagnosis

	// ClearStaleLock releases a stale disk lock for the given profile by
	// clearing stale state files and terminating the orphaned process(es) that
	// still hold the disk. It returns the number of processes terminated. It
	// refuses to act (returning an error) when a live manager process for the
	// instance exists, as that indicates a running VM rather than a stale lock.
	ClearStaleLock(profile string) (cleared int, err error)
}
