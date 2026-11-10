//go:build windows

package fix

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	fsctlLockVolume     = 0x00090018
	fsctlDismountVolume = 0x00090020
)

// openFlags bypass the OS cache (R7).
func openFlags() uint32 { return windows.FILE_FLAG_NO_BUFFERING | windows.FILE_FLAG_WRITE_THROUGH }

func unbufferedOK() error {
	if f := openFlags(); f&windows.FILE_FLAG_NO_BUFFERING == 0 || f&windows.FILE_FLAG_WRITE_THROUGH == 0 {
		return fmt.Errorf("flags %#x", f)
	}
	return nil
}

// openRaw locks and dismounts every volume of the card, then opens the physical disk for
// unbuffered, write-through I/O. Only reachable after Gate and Confirm.
func openRaw(t Target) (RW, io.Closer, error) {
	if !strings.HasPrefix(t.Disk.Path, `\\.\PhysicalDrive`) {
		return nil, nil, fmt.Errorf("not a physical disk: %q", t.Disk.Path)
	}
	var held []windows.Handle
	release := func() {
		for _, h := range held {
			windows.CloseHandle(h)
		}
	}
	for _, v := range t.Disk.Volumes {
		root := strings.TrimRight(v.Root, `\`)
		if len(root) != 2 {
			continue
		}
		p, _ := windows.UTF16PtrFromString(`\\.\` + root)
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("opening %s: %w", root, err)
		}
		held = append(held, h)
		var n uint32
		if err := windows.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &n, nil); err != nil {
			release()
			return nil, nil, fmt.Errorf("%s is in use — close programs using the card (%w)", root, err)
		}
		windows.DeviceIoControl(h, fsctlDismountVolume, nil, 0, nil, 0, &n, nil)
	}
	p, _ := windows.UTF16PtrFromString(t.Disk.Path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, openFlags(), 0)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("opening %s: %w", t.Disk.Path, err)
	}
	f := os.NewFile(uintptr(h), t.Disk.Path)
	return f, closer(func() error { err := f.Close(); release(); return err }), nil
}

type closer func() error

func (c closer) Close() error { return c() }
