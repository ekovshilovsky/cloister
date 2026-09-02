//go:build darwin

package broker

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

const (
	nativeACLAttribute         = "com.apple.system.Security"
	attrBitMapCount            = 5
	attrCommonExtendedSecurity = 0x00400000
	kauthFileSecNoACL          = ^uint32(0)
)

type attrList struct {
	BitmapCount uint16
	Reserved    uint16
	CommonAttr  uint32
	VolumeAttr  uint32
	DirAttr     uint32
	FileAttr    uint32
	ForkAttr    uint32
}

func listXattrs(path string) ([]string, error) {
	pointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	size, _, errno := syscall.Syscall6(syscall.SYS_LISTXATTR, uintptr(unsafe.Pointer(pointer)), 0, 0, 0, 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("listing extended attributes for %q: %w", path, errno)
	}
	var result []string
	if size > 0 {
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
		for _, name := range bytes.Split(buffer[:count], []byte{0}) {
			if len(name) > 0 {
				result = append(result, string(name))
			}
		}
	}
	hasACL, err := hasNativeACL(path)
	if err != nil {
		return nil, err
	}
	if hasACL && !slices.Contains(result, nativeACLAttribute) {
		result = append(result, nativeACLAttribute)
	}
	return result, nil
}

// hasNativeACL reads ATTR_CMN_EXTENDED_SECURITY through getattrlist(2).
// listxattr(2) does not enumerate native macOS ACLs even though their on-disk
// representation uses the com.apple.system.Security extended-security name.
func hasNativeACL(path string) (bool, error) {
	pointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes := attrList{
		BitmapCount: attrBitMapCount,
		CommonAttr:  attrCommonExtendedSecurity,
	}
	buffer := make([]byte, 4096)
	_, _, errno := syscall.Syscall6(
		syscall.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		0,
		0,
	)
	if errno != 0 {
		return false, fmt.Errorf("reading native macOS ACL for %q: %w", path, errno)
	}
	if len(buffer) < 4 {
		return false, fmt.Errorf("reading native macOS ACL for %q: truncated attribute buffer", path)
	}
	returned := int(binary.LittleEndian.Uint32(buffer[:4]))
	if returned == 4 {
		return false, nil
	}
	if returned < 12 || returned > len(buffer) {
		return false, fmt.Errorf("reading native macOS ACL for %q: invalid attribute buffer length %d", path, returned)
	}

	// Variable-length attributes are returned as an attrreference. Its signed
	// offset is relative to the reference itself, not the start of the buffer.
	reference := 4
	dataOffset := int(int32(binary.LittleEndian.Uint32(buffer[reference : reference+4])))
	dataLength := int(binary.LittleEndian.Uint32(buffer[reference+4 : reference+8]))
	dataStart := reference + dataOffset
	dataEnd := dataStart + dataLength
	if dataLength == 0 {
		return false, nil
	}
	if dataStart < 0 || dataEnd < dataStart || dataEnd > returned {
		return false, fmt.Errorf("reading native macOS ACL for %q: invalid extended-security reference", path)
	}

	// kauth_filesec begins with a magic word and two 16-byte GUIDs; the ACL's
	// entry count follows at byte 36. KAUTH_FILESEC_NOACL distinguishes the
	// absence of an ACL from an empty ACL, which still has permission semantics.
	const entryCountOffset = 36
	if dataLength < entryCountOffset+4 {
		return false, fmt.Errorf("reading native macOS ACL for %q: truncated extended-security data", path)
	}
	entryCount := binary.LittleEndian.Uint32(buffer[dataStart+entryCountOffset : dataStart+entryCountOffset+4])
	return entryCount != kauthFileSecNoACL, nil
}
