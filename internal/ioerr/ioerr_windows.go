package ioerr

import "syscall"

var (
	diskFull          = []syscall.Errno{112, 39}      // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	tooLarge          = []syscall.Errno{223}          // ERROR_FILE_TOO_LARGE
	locked            = []syscall.Errno{32, 33}       // SHARING_VIOLATION, LOCK_VIOLATION
	writeProtected    = []syscall.Errno{19}           // ERROR_WRITE_PROTECT
	permission        = []syscall.Errno{5}            // ERROR_ACCESS_DENIED
	gone              = []syscall.Errno{21, 1167, 55} // NOT_READY, DEVICE_NOT_CONNECTED, DEV_NOT_EXIST
	unrecognizedFS    = []syscall.Errno{1005}         // ERROR_UNRECOGNIZED_VOLUME
	goneIfRootMissing = []syscall.Errno{2, 3}         // FILE_NOT_FOUND, PATH_NOT_FOUND
)
