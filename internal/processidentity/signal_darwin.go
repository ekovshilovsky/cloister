//go:build darwin

package processidentity

import (
	"fmt"
	"syscall"
)

// Signal checks the kernel identity immediately before signaling. Darwin has
// no identity-qualified signaling primitive, so a residual PID-reuse window
// remains between these adjacent operations.
func Signal(pid int, expected Identity, signal syscall.Signal) error {
	if Observe(pid, expected).State != Ours {
		return fmt.Errorf("refusing to signal PID %d without matching kernel start time", pid)
	}
	return syscall.Kill(pid, signal)
}

// SignalProcessGroup authorizes a group signal using the recorded member's
// kernel identity, with the same Darwin residual window as Signal.
func SignalProcessGroup(groupPID, memberPID int, expected Identity, signal syscall.Signal) error {
	if Observe(memberPID, expected).State != Ours {
		return fmt.Errorf("refusing to signal process group %d without matching member identity", groupPID)
	}
	if group, err := syscall.Getpgid(memberPID); err != nil || group != groupPID {
		return fmt.Errorf("refusing to signal process group %d without matching member group", groupPID)
	}
	return syscall.Kill(-groupPID, signal)
}
