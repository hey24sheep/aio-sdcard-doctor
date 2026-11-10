//go:build darwin

package fix

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// rawPath is the character device that bypasses the buffer cache (R7): /dev/disk4 → /dev/rdisk4.
func rawPath(p string) string {
	if strings.HasPrefix(p, "/dev/disk") {
		return "/dev/r" + strings.TrimPrefix(p, "/dev/")
	}
	return p
}

func unbufferedOK() error {
	if rawPath("/dev/disk4") != "/dev/rdisk4" {
		return fmt.Errorf("rawPath %q", rawPath("/dev/disk4"))
	}
	return nil
}

// openRaw unmounts the card's volumes (diskutil unmountDisk) and opens its raw device uncached.
func openRaw(t Target) (RW, io.Closer, error) {
	if !strings.HasPrefix(t.Disk.Path, "/dev/disk") {
		return nil, nil, fmt.Errorf("not a disk: %q", t.Disk.Path)
	}
	if err := Exec(context.Background(), []string{"diskutil", "unmountDisk", t.Disk.Path}, io.Discard); err != nil {
		return nil, nil, fmt.Errorf("the card is in use — close programs using it (%w)", err)
	}
	f, err := os.OpenFile(rawPath(t.Disk.Path), os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1)
	return f, f, nil
}
