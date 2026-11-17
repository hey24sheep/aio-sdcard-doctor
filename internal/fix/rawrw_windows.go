//go:build windows

package fix

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	fsctlLockVolume           = 0x00090018
	fsctlDismountVolume       = 0x00090020
	ioctlVolumeGetDiskExtents = 0x00560000
)

// openFlags bypass the OS cache (R7).
func openFlags() uint32 { return windows.FILE_FLAG_NO_BUFFERING | windows.FILE_FLAG_WRITE_THROUGH }

func unbufferedOK() error {
	if f := openFlags(); f&windows.FILE_FLAG_NO_BUFFERING == 0 || f&windows.FILE_FLAG_WRITE_THROUGH == 0 {
		return fmt.Errorf("flags %#x", f)
	}
	return nil
}

// allVolumes lists every volume the OS knows as \\?\Volume{GUID}\, lettered or not.
func allVolumes() ([]string, error) {
	buf := make([]uint16, windows.MAX_PATH+1)
	h, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return nil, err
	}
	defer windows.FindVolumeClose(h)
	var out []string
	for {
		out = append(out, windows.UTF16ToString(buf))
		if err := windows.FindNextVolume(h, &buf[0], uint32(len(buf))); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				return out, nil
			}
			return out, err
		}
	}
}

// volumeExtents are the physical disk numbers a volume lies on (opened with no access: enough
// for the query, never for a read or write).
func volumeExtents(vol string) ([]uint32, error) {
	p, _ := windows.UTF16PtrFromString(strings.TrimRight(vol, `\`))
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	out := make([]byte, 8+24*8) // VOLUME_DISK_EXTENTS: count, then DISK_EXTENTs of 24 bytes
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlVolumeGetDiskExtents, nil, 0, &out[0], uint32(len(out)), &n, nil); err != nil {
		return nil, err
	}
	var ds []uint32
	for i := uint32(0); i < binary.LittleEndian.Uint32(out) && 8+24*i+4 <= n; i++ {
		ds = append(ds, binary.LittleEndian.Uint32(out[8+24*i:]))
	}
	return ds, nil
}

// cardVolumes are the volumes to lock and dismount before the whole disk is written: every one
// on the disk, lettered or not (OA9); the lettered ones listed when that can't be worked out.
func cardVolumes(t Target) []string {
	if n, ok := diskNumber(t.Disk.Path); ok {
		if vs, err := volumesOnDisk(allVolumes, volumeExtents, n); err == nil {
			for i := range vs {
				vs[i] = strings.TrimRight(vs[i], `\`) // the volume itself, not its root folder
			}
			return vs
		}
	}
	var vs []string
	for _, v := range t.Disk.Volumes {
		if root := strings.TrimRight(v.Root, `\`); len(root) == 2 {
			vs = append(vs, `\\.\`+root)
		}
	}
	return vs
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
	for _, vol := range cardVolumes(t) {
		p, _ := windows.UTF16PtrFromString(vol)
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("opening %s: %w", vol, err)
		}
		held = append(held, h)
		var n uint32
		if err := windows.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &n, nil); err != nil {
			release()
			return nil, nil, fmt.Errorf("%s is in use — close programs using the card (%w)", vol, err)
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
