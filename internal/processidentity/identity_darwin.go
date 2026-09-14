//go:build darwin

package processidentity

/*
#include <libproc.h>
#include <sys/proc.h>
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const procPIDPathSize = 4096

func readNative(pid int) (Identity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Identity{}, fmt.Errorf("reading start time for PID %d: %w", pid, err)
	}
	start := info.Proc.P_starttime
	if info.Proc.P_stat == C.SZOMB {
		return Identity{}, fmt.Errorf("PID %d has exited", pid)
	}
	pathBuffer := make([]byte, procPIDPathSize)
	length := C.proc_pidpath(C.int(pid), unsafe.Pointer(&pathBuffer[0]), C.uint32_t(len(pathBuffer)))
	if length <= 0 {
		return Identity{}, fmt.Errorf("reading executable for PID %d", pid)
	}
	executable := strings.TrimRight(string(pathBuffer[:int(length)]), "\x00")
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Identity{}, fmt.Errorf("resolving executable for PID %d: %w", pid, err)
	}
	return Identity{
		StartTime:  fmt.Sprintf("%d:%d", start.Sec, start.Usec),
		Executable: resolved,
	}, nil
}
