//go:build linux

package processidentity

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func openOwnedPIDFD(pid int, expected Identity) (int, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			if Observe(pid, expected).State != Ours {
				return -1, fmt.Errorf("refusing to signal PID %d without matching kernel start time", pid)
			}
			return -1, nil
		}
		return -1, err
	}
	if Observe(pid, expected).State != Ours {
		unix.Close(fd)
		return -1, fmt.Errorf("refusing to signal PID %d without matching kernel start time", pid)
	}
	return fd, nil
}

// Signal uses a pidfd where the kernel supports it, closing the identity-check
// to signal PID-reuse window. Older kernels fall back to adjacent operations.
func Signal(pid int, expected Identity, signal syscall.Signal) error {
	fd, err := openOwnedPIDFD(pid, expected)
	if err != nil {
		return err
	}
	if fd < 0 {
		return syscall.Kill(pid, signal)
	}
	defer unix.Close(fd)
	return unix.PidfdSendSignal(fd, signal, nil, 0)
}

// SignalProcessGroup pins memberPID with a pidfd and rechecks both its identity
// and group immediately before signaling. The final raw group signal retains a
// narrow group-ID reuse window after that adjacent verification because Linux
// has no pidfd operation for a process group.
func SignalProcessGroup(groupPID, memberPID int, expected Identity, signal syscall.Signal) error {
	fd, err := openOwnedPIDFD(memberPID, expected)
	if err != nil {
		return err
	}
	if fd >= 0 {
		defer unix.Close(fd)
	}
	if Observe(memberPID, expected).State != Ours {
		return fmt.Errorf("refusing to signal process group %d without matching member identity", groupPID)
	}
	if fd >= 0 {
		if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
			return err
		}
	}
	if group, groupErr := syscall.Getpgid(memberPID); groupErr != nil || group != groupPID {
		return fmt.Errorf("refusing to signal process group %d without matching member group", groupPID)
	}
	return syscall.Kill(-groupPID, signal)
}
