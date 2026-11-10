package rescue

import "syscall"

var diskFullErrno = syscall.Errno(112) // ERROR_DISK_FULL
