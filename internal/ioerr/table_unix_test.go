//go:build !windows

package ioerr

import "syscall"

func codeTable() map[string][]uintptr {
	return map[string][]uintptr{
		"DiskFull": {uintptr(syscall.ENOSPC), uintptr(syscall.EDQUOT)}, "TooLarge": {uintptr(syscall.EFBIG)},
		"Locked": {uintptr(syscall.EBUSY), uintptr(syscall.EAGAIN)}, "WriteProtected": {uintptr(syscall.EROFS)},
		"Permission": {uintptr(syscall.EACCES), uintptr(syscall.EPERM)}, "Gone": {uintptr(syscall.ENODEV), uintptr(syscall.ENXIO)},
		"UnrecognizedFS": {},
	}
}

func rootMissingCode() uintptr { return uintptr(syscall.EIO) }
