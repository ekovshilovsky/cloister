//go:build darwin

package processidentity

/*
#include <libproc.h>
#include <sys/proc.h>
#include <sys/resource.h>
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
		return Identity{}, fmt.Errorf("reading state for PID %d: %w", pid, err)
	}
	if info.Proc.P_stat == C.SZOMB {
		return Identity{}, fmt.Errorf("PID %d has exited", pid)
	}
	var usage C.struct_rusage_info_v3
	if C.proc_pid_rusage(C.int(pid), C.RUSAGE_INFO_V3, (*C.rusage_info_t)(unsafe.Pointer(&usage))) != 0 {
		return Identity{}, fmt.Errorf("reading boot-relative start time for PID %d", pid)
	}
	pathBuffer := make([]byte, procPIDPathSize)
	length := C.proc_pidpath(C.int(pid), unsafe.Pointer(&pathBuffer[0]), C.uint32_t(len(pathBuffer)))
	executable := ""
	if length > 0 {
		executable = strings.TrimRight(string(pathBuffer[:int(length)]), "\x00")
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
	}
	return Identity{
		StartTime:  fmt.Sprintf("%d", uint64(usage.ri_proc_start_abstime)),
		Executable: executable,
	}, nil
}
