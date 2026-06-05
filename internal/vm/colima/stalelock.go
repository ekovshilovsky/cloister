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
// it cannot attach the instance disk because an orphaned VM process still holds
// it locked. A host crash that skips Lima's clean-shutdown path is the usual
// cause: the disk attachment lock is never released, so the next start fails.
var staleLockMarkers = []string{
	"in use by instance",
	"failed to run attach disk",
}

// gracefulKillTimeout bounds how long ClearStaleLock waits for a process to
// exit after SIGTERM before escalating to SIGKILL.
const gracefulKillTimeout = 5 * time.Second

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
// diskresize.go and resolves to <limaHome>/<limaInstanceName>.
func limaInstanceName(profile string) string {
	return limaSubdirPrefix + VMName(profile)
}

// DiagnoseStartFailure implements vm.StaleLockRecoverer. It returns a diagnosis
// only when the most recent start failure is attributable to a stale disk lock:
// the hostagent stderr log carries a lock marker AND no live hostagent manages
// the instance. The hostagent check is the safety gate — a genuinely running VM
// always has a live hostagent, so this never offers to kill a healthy VM.
func (b *Backend) DiagnoseStartFailure(profile string) *vm.StaleLockDiagnosis {
	dir, err := limaInstanceDir(profile)
	if err != nil {
		return nil
	}
	if !haStderrHasStaleLockMarker(filepath.Join(dir, "ha.stderr.log")) {
		return nil
	}

	instance := limaInstanceName(profile)
	if hostagentAliveFor(instance) {
		// A live manager means the VM is actually running or being managed;
		// this is some other failure, not an abandoned lock.
		return nil
	}

	disk := filepath.Join(dir, "disk")
	holders := diskHolderPIDs(disk)
	return &vm.StaleLockDiagnosis{
		VMName:     instance,
		DiskPath:   disk,
		OrphanPIDs: holders,
		Summary:    staleLockSummary(instance, disk, holders),
	}
}

// ClearStaleLock implements vm.StaleLockRecoverer. It clears stale Colima state
// files and terminates any process still holding the instance disk, then
// verifies the disk has been released. It refuses to act when a live hostagent
// manages the instance, which would indicate a running VM rather than a stale
// lock.
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

	// Clear stale PID/socket/tmp files. Colima reports the instance as already
	// stopped here (its hostagent died in the crash), so this only tidies state
	// files and never returns a meaningful error for our purposes.
	_, _ = runColima(false, "stop", "--force", "--profile", VMName(profile))

	// Terminate the orphaned process(es) still holding the disk. SIGTERM first
	// so Virtualization.framework performs an orderly power-off (cleanest for
	// the guest's journaled filesystem); escalate to SIGKILL only if needed.
	killed := 0
	for _, pid := range diskHolderPIDs(disk) {
		if killProcess(pid) {
			killed++
		}
	}

	if remaining := diskHolderPIDs(disk); len(remaining) > 0 {
		return killed, fmt.Errorf("disk %s is still held by process(es) %v after recovery", disk, remaining)
	}
	return killed, nil
}

// CleanupStaleLocks scans every cloister-managed Colima instance and clears any
// whose disk is held by an orphaned process (no live hostagent). It returns the
// number of locks cleared along with a human-readable report line per instance
// acted upon. It is the manual counterpart to the automatic recovery offered on
// the start path, surfaced through `cloister cleanup`.
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
		if len(holders) == 0 || hostagentAliveFor(instance) {
			continue
		}

		colimaProfile := strings.TrimPrefix(instance, limaSubdirPrefix)
		_, _ = runColima(false, "stop", "--force", "--profile", colimaProfile)
		killedHere := 0
		for _, pid := range holders {
			if killProcess(pid) {
				killedHere++
			}
		}
		if killedHere > 0 {
			cleared++
			report = append(report, fmt.Sprintf("%s: cleared stale lock, terminated %d orphaned process(es)", instance, killedHere))
		}
	}
	return cleared, report, nil
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
// lock, including the concrete instance, disk, and holder PIDs.
func staleLockSummary(instance, disk string, holders []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A previous VM for %q did not shut down cleanly (typically a host crash).\n", instance)
	b.WriteString("An orphaned process is still holding its disk locked, so the VM cannot start.\n")
	fmt.Fprintf(&b, "  disk:          %s\n", disk)
	if len(holders) > 0 {
		fmt.Fprintf(&b, "  holder PID(s): %v\n", holders)
	} else {
		b.WriteString("  holder PID(s): none (only stale state files remain)\n")
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
