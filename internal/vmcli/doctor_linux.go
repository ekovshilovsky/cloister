//go:build linux

package vmcli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// platformChecks returns Linux-specific diagnostic checks for swap, disk space,
// and memory availability. These rely on /proc and syscall.Statfs which are
// only available on Linux.
func platformChecks() []struct {
	name string
	fn   func() CheckResult
} {
	return []struct {
		name string
		fn   func() CheckResult
	}{
		{"swap", checkSwap},
		{"disk", checkDisk},
		{"memory", checkMemory},
	}
}

// checkSwap reads /proc/swaps to determine whether swap space is configured.
// Having swap enabled helps prevent out-of-memory kills in memory-constrained
// VM environments.
func checkSwap() CheckResult {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return CheckResult{
			Name:   "swap",
			Status: "warn",
			Detail: "/proc/swaps not readable",
		}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}

	// The first line is the header; any additional lines indicate active swap devices.
	if lineCount > 1 {
		return CheckResult{
			Name:   "swap",
			Status: "pass",
			Detail: "swap configured",
		}
	}
	return CheckResult{
		Name:   "swap",
		Status: "warn",
		Detail: "no swap configured",
	}
}

// checkDisk uses syscall.Statfs to check available disk space on the root
// filesystem. Returns a warning when less than 10% of total space is free.
func checkDisk() CheckResult {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return CheckResult{
			Name:   "disk",
			Status: "warn",
			Detail: fmt.Sprintf("unable to stat filesystem: %v", err),
		}
	}

	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)

	if totalBytes == 0 {
		return CheckResult{
			Name:   "disk",
			Status: "warn",
			Detail: "unable to determine disk size",
		}
	}

	pctFree := float64(freeBytes) / float64(totalBytes) * 100
	detail := fmt.Sprintf("%.0f%% free (%.1f GB / %.1f GB)",
		pctFree,
		float64(freeBytes)/1e9,
		float64(totalBytes)/1e9,
	)

	if pctFree < 10 {
		return CheckResult{
			Name:   "disk",
			Status: "warn",
			Detail: detail,
		}
	}
	return CheckResult{
		Name:   "disk",
		Status: "pass",
		Detail: detail,
	}
}

// checkMemory reads /proc/meminfo to determine total and available memory.
// Returns a warning when less than 20% of total memory is available, which
// may indicate resource pressure in the VM.
func checkMemory() CheckResult {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return CheckResult{
			Name:   "memory",
			Status: "warn",
			Detail: "/proc/meminfo not readable",
		}
	}
	defer f.Close()

	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMeminfoKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMeminfoKB(line)
		}
	}

	if memTotal == 0 {
		return CheckResult{
			Name:   "memory",
			Status: "warn",
			Detail: "unable to parse memory info",
		}
	}

	pctAvail := float64(memAvailable) / float64(memTotal) * 100
	detail := fmt.Sprintf("%.0f%% available (%.1f GB / %.1f GB)",
		pctAvail,
		float64(memAvailable)/1024/1024,
		float64(memTotal)/1024/1024,
	)

	if pctAvail < 20 {
		return CheckResult{
			Name:   "memory",
			Status: "warn",
			Detail: detail,
		}
	}
	return CheckResult{
		Name:   "memory",
		Status: "pass",
		Detail: detail,
	}
}

// parseMeminfoKB extracts the numeric kB value from a /proc/meminfo line such
// as "MemTotal:       16384000 kB".
func parseMeminfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return val
}
