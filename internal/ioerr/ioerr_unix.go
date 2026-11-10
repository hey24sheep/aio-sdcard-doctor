//go:build !windows

package ioerr

import "syscall"

var (
	diskFull          = []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT}
	tooLarge          = []syscall.Errno{syscall.EFBIG}
	locked            = []syscall.Errno{syscall.EBUSY, syscall.EAGAIN}
	writeProtected    = []syscall.Errno{syscall.EROFS}
	permission        = []syscall.Errno{syscall.EACCES, syscall.EPERM}
	gone              = []syscall.Errno{syscall.ENODEV, syscall.ENXIO}
	unrecognizedFS    = []syscall.Errno{}
	goneIfRootMissing = []syscall.Errno{syscall.EIO, syscall.ENOENT}
)
