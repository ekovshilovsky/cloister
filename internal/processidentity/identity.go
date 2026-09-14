package processidentity

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Identity is PID-reuse-safe process metadata captured from the host kernel.
type Identity struct {
	StartTime  string `json:"start_time"`
	Executable string `json:"executable"`
}

// State distinguishes process liveness from ownership. Unverifiable means the
// PID exists, but the kernel start time could not be read safely.
type State uint8

const (
	Unverifiable State = iota
	Dead
	NotOurs
	Ours
)

// Observation is a PID's ownership state and the reason when it is
// unverifiable.
type Observation struct {
	State   State
	Current Identity
	Err     error
}

// Read returns the kernel start time and best-effort resolved executable for
// pid. Start time is required; executable metadata may be unavailable.
func Read(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("invalid process PID %d", pid)
	}
	var identity Identity
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		identity, err = readNative(pid)
		if err == nil && identity.StartTime != "" {
			return identity, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return Identity{}, err
	}
	return Identity{}, fmt.Errorf("incomplete process identity for PID %d", pid)
}

// Observe classifies pid without treating an unreadable identity as dead.
// Kernel start time is the deciding ownership field; executable is retained as
// corroborating diagnostic metadata because its path can drift across upgrades.
func Observe(pid int, expected Identity) Observation {
	if pid <= 0 {
		return Observation{State: Dead}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Observation{State: Dead}
		}
		if !errors.Is(err, syscall.EPERM) {
			return Observation{State: Unverifiable, Err: fmt.Errorf("checking PID %d: %w", pid, err)}
		}
	}
	if expected.StartTime == "" {
		return Observation{State: Unverifiable, Err: fmt.Errorf("PID %d has no recorded kernel start time", pid)}
	}
	current, err := Read(pid)
	if err != nil {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return Observation{State: Dead}
		}
		return Observation{State: Unverifiable, Err: err}
	}
	if current.StartTime != expected.StartTime {
		return Observation{State: NotOurs, Current: current}
	}
	return Observation{State: Ours, Current: current}
}

// Matches re-reads pid and requires exact kernel start-time equality.
func Matches(pid int, expected Identity) bool {
	return Observe(pid, expected).State == Ours
}

// Kill signals pid only while it still has the captured kernel identity.
func Kill(pid int, expected Identity) error {
	return Signal(pid, expected, syscall.SIGKILL)
}
