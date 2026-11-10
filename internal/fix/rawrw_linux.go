//go:build linux

package fix

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// openFlags bypass the page cache (R7): O_DIRECT needs the aligned buffers health.go uses.
func openFlags() int { return os.O_RDWR | unix.O_DIRECT | unix.O_SYNC }

func unbufferedOK() error {
	if openFlags()&unix.O_DIRECT == 0 {
		return fmt.Errorf("no O_DIRECT")
	}
	return nil
}

// openRaw opens the whole card for the health test; every volume must be unmounted first.
func openRaw(t Target) (RW, io.Closer, error) {
	for _, v := range t.Disk.Volumes {
		if v.Root != "" {
			return nil, nil, fmt.Errorf("unmount the card first (umount %s), then run this again", v.Root)
		}
	}
	f, err := os.OpenFile(t.Disk.Path, openFlags(), 0)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}
