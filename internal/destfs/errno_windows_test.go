package destfs

import "syscall"

var lockedErrno = syscall.Errno(32) // ERROR_SHARING_VIOLATION
