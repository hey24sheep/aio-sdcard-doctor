//go:build !windows

package rescue

import "syscall"

var diskFullErrno = syscall.ENOSPC
