// Package ioerr classifies OS I/O errors into the few kinds the actions react to.
package ioerr

import (
	"errors"
	"os"
	"syscall"
)

func errno(err error) (syscall.Errno, bool) {
	var e syscall.Errno
	if err != nil && errors.As(err, &e) {
		return e, true
	}
	return 0, false
}

func in(err error, codes []syscall.Errno) bool {
	e, ok := errno(err)
	if !ok {
		return false
	}
	for _, c := range codes {
		if e == c {
			return true
		}
	}
	return false
}

// IsDiskFull reports a destination out of space.
func IsDiskFull(err error) bool { return in(err, diskFull) }

// IsTooLarge reports a file larger than the file system allows (FAT32 4 GiB).
func IsTooLarge(err error) bool { return in(err, tooLarge) }

// IsLocked reports a file or volume held by another process.
func IsLocked(err error) bool { return in(err, locked) }

// IsWriteProtected reports a read-only medium.
func IsWriteProtected(err error) bool { return in(err, writeProtected) }

// IsPermission reports access denied.
func IsPermission(err error) bool { return in(err, permission) }

// IsGone reports a device that disappeared (unplugged, not ready).
func IsGone(err error) bool { return in(err, gone) }

// IsUnrecognizedFS reports a volume the OS can't read (RAW).
func IsUnrecognizedFS(err error) bool { return in(err, unrecognizedFS) }

// IsGoneAt is IsGone, plus "not found"-style errors when the source root itself has vanished.
func IsGoneAt(err error, root string) bool {
	if IsGone(err) {
		return true
	}
	if !in(err, goneIfRootMissing) {
		return false
	}
	_, statErr := os.Stat(root)
	return statErr != nil
}
