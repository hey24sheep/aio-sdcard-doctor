package destfs

import (
	"os"

	"golang.org/x/sys/windows"
)

// syncDir is a no-op on Windows: directories can't be fsync'd and NTFS journals metadata.
func syncDir(string) error { return nil }

// makeSparse marks a new image file sparse so out-of-order writes don't zero-fill the gaps (R21).
// Failure (FAT/exFAT destinations) is harmless.
func makeSparse(f *os.File) {
	var n uint32
	windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil)
}

func invokingUser() *owner { return nil }

var lchown = func(string, int, int) error { return nil }
