//go:build linux

package processidentity

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readNative(pid int) (Identity, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Identity{}, fmt.Errorf("reading start time for PID %d: %w", pid, err)
	}
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 {
		return Identity{}, fmt.Errorf("parsing start time for PID %d", pid)
	}
	fields := strings.Fields(string(stat[closing+1:]))
	// After the parenthesized command, index zero is field 3 (state), so
	// Linux's field 22 (starttime in clock ticks since boot) is index 19.
	if len(fields) <= 19 {
		return Identity{}, fmt.Errorf("parsing start time for PID %d", pid)
	}
	if fields[0] == "Z" {
		return Identity{}, fmt.Errorf("PID %d has exited", pid)
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return Identity{}, fmt.Errorf("parsing start time for PID %d: %w", pid, err)
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		executable = ""
	} else {
		executable = strings.TrimSuffix(executable, " (deleted)")
	}
	return Identity{StartTime: fields[19], Executable: executable}, nil
}
