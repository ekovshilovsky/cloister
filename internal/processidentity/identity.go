package processidentity

import (
	"fmt"
	"os"
	"time"
)

// Identity is PID-reuse-safe process metadata captured from the host kernel.
type Identity struct {
	StartTime  string `json:"start_time"`
	Executable string `json:"executable"`
}

// Read returns the kernel start time and resolved executable for pid.
func Read(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("invalid process PID %d", pid)
	}
	var identity Identity
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		identity, err = readNative(pid)
		if err == nil && identity.StartTime != "" && identity.Executable != "" {
			return identity, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return Identity{}, err
	}
	return Identity{}, fmt.Errorf("incomplete process identity for PID %d", pid)
}

// Matches re-reads pid and requires exact start-time and executable equality.
func Matches(pid int, expected Identity) bool {
	if expected.StartTime == "" || expected.Executable == "" {
		return false
	}
	current, err := Read(pid)
	return err == nil && current == expected
}

// Kill signals pid only while it still has the captured kernel identity.
func Kill(pid int, expected Identity) error {
	if !Matches(pid, expected) {
		return fmt.Errorf("refusing to signal PID %d without exact process identity", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
