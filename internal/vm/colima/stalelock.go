package colima

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloister.io/internal/vm"
)

// limaSubdirPrefix is the prefix Colima prepends to its own profile name when
// creating the underlying Lima instance directory beneath LIMA_HOME. A cloister
// profile "innolumi" maps to Colima profile "cloister-innolumi" (see VMName),
// which Colima realizes as Lima instance "colima-cloister-innolumi".
const limaSubdirPrefix = "colima-"

// staleLockMarkers are substrings Lima writes to the hostagent stderr log when
// it cannot attach the instance disk because it is still locked. A host crash
// that skips Lima's clean-shutdown path is the usual cause: the lock is never
// released, so the next start fails.
var staleLockMarkers = []string{
	"in use by instance",
	"failed to run attach disk",
}

// gracefulKillTimeout bounds how long recovery waits for a process to exit
// after SIGTERM before escalating to SIGKILL.
const gracefulKillTimeout = 5 * time.Second

// A stale disk lock manifests in one of two ways after an unclean shutdown, and
// recovery must address both:
//
//   - Process orphan: a live Apple Virtualization VM process still holds the
//     disk image open. `lsof` finds it; the fix is to terminate it.
//   - Lima registry lock: a force-stopped or crashed instance leaves an
//     "in_use_by" symlink in Lima's _disks registry with no process behind it.
//     `lsof` finds nothing; the fix is `limactl disk unlock`.
//
// The authoritative "this is stale" signal is: the disk is marked in use by an
// instance that has no live hostagent managing it.

// limaHome returns Colima's LIMA_HOME directory (~/.colima/_lima), where every
// instance keeps its disk image, state files, and hostagent logs.
func limaHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".colima", "_lima"), nil
}

// limaInstanceName returns the Lima instance name backing a cloister profile.
// The matching instance directory helper, limaInstanceDir, lives in
// diskresize.go and resolves to <limaHome>/<limaInstanceName>. Colima names the
// instance's data disk identically, so this also serves as the Lima disk name.
func limaInstanceName(profile string) string {
	return limaSubdirPrefix + VMName(profile)
}

// inUseBySymlinkPath returns the path to the Lima _disks "in_use_by" symlink for
// an instance's data disk. Its presence records that Lima considers the disk
// locked by some instance.
func inUseBySymlinkPath(instance string) (string, error) {
	home, err := limaHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "_disks", instance, "in_use_by"), nil
}

// diskRegistryLocked reports whether Lima's _disks registry currently marks the
// instance's data disk as in use (via the in_use_by symlink). Combined with a
// hostagent-liveness check, a true result for a non-running instance indicates a
// stale lock.
func diskRegistryLocked(instance string) bool {
	path, err := inUseBySymlinkPath(instance)
	if err != nil {
		return false
	}
	// Lstat (not Stat) so a dangling symlink still counts as present.
	if _, err := os.Lstat(path); err == nil {
		return true
	}
	return false
}

// needsRecovery decides whether an instance has a stale lock that recovery
// should clear. It is a pure helper to keep the policy testable: an instance is
// recoverable only when no hostagent manages it (so it is not actually running)
// and either a process still holds its disk or the Lima registry marks it
// locked.
func needsRecovery(hostagentAlive bool, holderCount int, registryLocked bool) bool {
	if hostagentAlive {
		return false
	}
	return holderCount > 0 || registryLocked
}

// DiagnoseStartFailure implements vm.StaleLockRecoverer. It returns a diagnosis
// when the most recent start failure is attributable to a stale disk lock —
// either the hostagent stderr log carries a lock marker or Lima's registry
// still marks the disk in use — AND no live hostagent manages the instance. The
// hostagent check is the safety gate: a genuinely running VM always has a live
// hostagent, so this never offers to recover a healthy VM.
func (b *Backend) DiagnoseStartFailure(profile string) *vm.StaleLockDiagnosis {
	dir, err := limaInstanceDir(profile)
	if err != nil {
		return nil
	}
	instance := limaInstanceName(profile)

	markerPresent := haStderrHasStaleLockMarker(filepath.Join(dir, "ha.stderr.log"))
	registryLocked := diskRegistryLocked(instance)
	if !markerPresent && !registryLocked {
		return nil
	}

	disk := filepath.Join(dir, "disk")
	holders := diskHolderPIDs(disk)
	if !needsRecovery(hostagentAliveFor(instance), len(holders), registryLocked) {
		return nil
	}

	return &vm.StaleLockDiagnosis{
		VMName:     instance,
		DiskPath:   disk,
		OrphanPIDs: holders,
		Summary:    staleLockSummary(instance, disk, holders, registryLocked),
	}
}

// ClearStaleLock implements vm.StaleLockRecoverer. It clears stale Colima state
// files, terminates any process still holding the instance disk, releases Lima's
// disk-registry lock, then verifies the lock is gone. It refuses to act when a
// live hostagent manages the instance, which would indicate a running VM rather
// than a stale lock.
func (b *Backend) ClearStaleLock(profile string) (int, error) {
	instance := limaInstanceName(profile)
	if hostagentAliveFor(instance) {
		return 0, fmt.Errorf("instance %q has a live hostagent; refusing to clear a lock for a running VM", instance)
	}

	dir, err := limaInstanceDir(profile)
	if err != nil {
		return 0, err
	}
	disk := filepath.Join(dir, "disk")

	killed := clearInstanceLock(VMName(profile), instance, disk)

	if remaining := diskHolderPIDs(disk); len(remaining) > 0 {
		return killed, fmt.Errorf("disk %s is still held by process(es) %v after recovery", disk, remaining)
	}
	if diskRegistryLocked(instance) {
		return killed, fmt.Errorf("Lima still reports disk %q as in use after recovery", instance)
	}
	return killed, nil
}

// CleanupStaleLocks scans every cloister-managed Colima instance and clears any
// with a stale disk lock (a held disk or a Lima registry lock, with no live
// hostagent). It returns the number of locks cleared along with a human-readable
// report line per instance acted upon. It is the manual counterpart to the
// automatic recovery offered on the start path, surfaced through `cloister
// cleanup`.
func (b *Backend) CleanupStaleLocks() (int, []string, error) {
	home, err := limaHome()
	if err != nil {
		return 0, nil, err
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		// No LIMA_HOME yet means nothing to clean — not an error.
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}

	cleared := 0
	var report []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		instance := e.Name()
		// Only consider cloister-managed Colima instances.
		if !strings.HasPrefix(instance, limaSubdirPrefix+vmPrefix) {
			continue
		}
		disk := filepath.Join(home, instance, "disk")
		holders := diskHolderPIDs(disk)
		if !needsRecovery(hostagentAliveFor(instance), len(holders), diskRegistryLocked(instance)) {
			continue
		}

		colimaProfile := strings.TrimPrefix(instance, limaSubdirPrefix)
		killed := clearInstanceLock(colimaProfile, instance, disk)
		cleared++
		if killed > 0 {
			report = append(report, fmt.Sprintf("%s: cleared stale lock, terminated %d orphaned process(es)", instance, killed))
		} else {
			report = append(report, fmt.Sprintf("%s: cleared stale Lima disk lock", instance))
		}
	}
	return cleared, report, nil
}

// clearInstanceLock performs the recovery steps shared by ClearStaleLock and
// CleanupStaleLocks: clear stale state files, terminate any process holding the
// disk, and release Lima's disk-registry lock. It returns the number of
// processes terminated. colimaProfile is the Colima --profile value; instance is
// the Lima instance/disk name; disk is the OS disk image path.
func clearInstanceLock(colimaProfile, instance, disk string) int {
	// Clear stale PID/socket/tmp files. Colima reports the instance as already
	// stopped here (its hostagent died), so this only tidies state files.
	_, _ = runColima(false, "stop", "--force", "--profile", colimaProfile)

	// Terminate any process still holding the disk. SIGTERM first so
	// Virtualization.framework performs an orderly power-off (cleanest for the
	// guest's journaled filesystem); escalate to SIGKILL only if needed. A
	// process must be gone before Lima will release the disk lock.
	killed := 0
	for _, pid := range diskHolderPIDs(disk) {
		if killProcess(pid) {
			killed++
		}
	}

	// Release Lima's disk-registry lock (the in_use_by symlink). This is the
	// fix for the no-process variant left by a force-stop or crash, and is a
	// safe no-op when the disk was not registry-locked.
	if diskRegistryLocked(instance) {
		_ = unlockLimaDisk(instance)
	}
	return killed
}

// unlockLimaDisk releases Lima's lock on a named disk via `limactl disk unlock`.
// LIMA_HOME is set explicitly because cloister drives Colima's Lima home
// (~/.colima/_lima), not the default ~/.lima.
func unlockLimaDisk(diskName string) error {
	home, err := limaHome()
	if err != nil {
		return err
	}
	cmd := exec.Command("limactl", "disk", "unlock", diskName)
	cmd.Env = append(os.Environ(), "LIMA_HOME="+home)
	return cmd.Run()
}

// haStderrHasStaleLockMarker reports whether the tail of the hostagent stderr
// log contains a stale-lock marker. Only the tail is inspected so a marker from
// a long-resolved earlier failure cannot trigger a false positive; the file is
// read right after a failed start, so its latest content reflects that attempt.
func haStderrHasStaleLockMarker(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	const tail = 8192
	s := string(data)
	if len(s) > tail {
		s = s[len(s)-tail:]
	}
	return containsStaleLockMarker(s)
}

// containsStaleLockMarker reports whether s contains any known stale-lock
// marker. It is a pure helper to keep marker matching unit-testable.
func containsStaleLockMarker(s string) bool {
	for _, m := range staleLockMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// diskHolderPIDs returns the PIDs of processes that currently hold the given
// disk file open, via `lsof -t`. lsof exits non-zero when no process holds the
// file; that is the expected "nobody holds it" case, so the error is ignored
// and an empty slice is returned.
func diskHolderPIDs(disk string) []int {
	out, _ := exec.Command("lsof", "-t", "--", disk).Output()
	return parsePIDList(string(out))
}

// parsePIDList parses whitespace-separated PIDs (as emitted by `lsof -t`) into a
// slice of positive integers. It is a pure helper to keep parsing testable.
func parsePIDList(out string) []int {
	var pids []int
	for _, f := range strings.Fields(out) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	return pids
}

// hostagentAliveFor reports whether a live `limactl hostagent` process is
// managing the given Lima instance. A managed instance is a running (or
// actively starting) VM, never a stale lock.
func hostagentAliveFor(instance string) bool {
	out, _ := exec.Command("ps", "-axo", "command=").Output()
	return psHasHostagentFor(string(out), instance)
}

// psHasHostagentFor reports whether any line of `ps` output is a limactl
// hostagent invocation for the given instance. The instance name appears as a
// standalone final argument, so an exact field match avoids false positives
// between profiles whose names share a prefix (e.g. "work" vs "work2"). It is a
// pure helper to keep matching testable.
func psHasHostagentFor(psOut, instance string) bool {
	for _, line := range strings.Split(psOut, "\n") {
		if !strings.Contains(line, "limactl") || !strings.Contains(line, "hostagent") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == instance {
				return true
			}
		}
	}
	return false
}

// staleLockSummary renders the user-facing explanation for a diagnosed stale
// lock, naming the concrete instance, disk, holders, and lock kind.
func staleLockSummary(instance, disk string, holders []int, registryLocked bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A previous VM for %q did not shut down cleanly (typically a host crash).\n", instance)
	b.WriteString("Its disk is still locked, so the VM cannot start.\n")
	fmt.Fprintf(&b, "  disk:          %s\n", disk)
	if len(holders) > 0 {
		fmt.Fprintf(&b, "  held by:       process(es) %v\n", holders)
	} else if registryLocked {
		b.WriteString("  held by:       a stale Lima disk-registry lock (no process)\n")
	} else {
		b.WriteString("  held by:       stale state files only\n")
	}
	return b.String()
}

// killProcess terminates a process gracefully (SIGTERM) and escalates to
// SIGKILL if it has not exited within gracefulKillTimeout. It returns true when
// the process is no longer alive afterward.
func killProcess(pid int) bool {
	if !processAlive(pid) {
		return true
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)

	deadline := time.Now().Add(gracefulKillTimeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return !processAlive(pid)
}

// processAlive reports whether a process with the given PID currently exists,
// using the canonical signal-0 liveness probe.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
