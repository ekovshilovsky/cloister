// Proprietary and confidential. All rights reserved.

//go:build darwin

package broker

import (
	"bytes"
	"fmt"
	"syscall"
	"unsafe"
)

func listXattrs(path string) ([]string, error) {
	pointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	size, _, errno := syscall.Syscall6(syscall.SYS_LISTXATTR, uintptr(unsafe.Pointer(pointer)), 0, 0, 0, 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("listing extended attributes for %q: %w", path, errno)
	}
	if size == 0 {
		return nil, nil
	}
	buffer := make([]byte, size)
	count, _, errno := syscall.Syscall6(
		syscall.SYS_LISTXATTR,
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("reading extended attributes for %q: %w", path, errno)
	}
	var result []string
	for _, name := range bytes.Split(buffer[:count], []byte{0}) {
		if len(name) > 0 {
			result = append(result, string(name))
		}
	}
	return result, nil
}
