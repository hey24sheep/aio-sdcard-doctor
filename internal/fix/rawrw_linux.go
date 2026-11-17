//go:build linux

package fix

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// openFlags bypass the page cache (R7): O_DIRECT needs the aligned buffers health.go uses.
// O_EXCL on a block device makes the kernel refuse it (EBUSY) while any partition of it is
// mounted or claimed: the "unmounted" seen at Plan is checked again at the open itself (SA-6).
func openFlags() int { return os.O_RDWR | unix.O_DIRECT | unix.O_SYNC | unix.O_EXCL }

func unbufferedOK() error {
	if openFlags()&unix.O_DIRECT == 0 {
		return fmt.Errorf("no O_DIRECT")
	}
	return nil
}

// openFile opens the card (a seam for tests).
var openFile = os.OpenFile

// openRaw opens the whole card for the health test; every volume must be unmounted first.
func openRaw(t Target) (RW, io.Closer, error) {
	for _, v := range t.Disk.Volumes {
		if v.Root != "" {
			return nil, nil, fmt.Errorf("unmount the card first (umount %s), then run this again", v.Root)
		}
	}
	f, err := openFile(t.Disk.Path, openFlags(), 0)
	if errors.Is(err, unix.EBUSY) {
		return nil, nil, fmt.Errorf("the card is in use (mounted, or held by another program) — unmount it first (sudo umount %s*), then run this again", t.Disk.Path)
	}
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}
