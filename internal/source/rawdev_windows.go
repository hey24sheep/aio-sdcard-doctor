package source

import (
	"encoding/binary"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetLengthInfo = 0x7405C
	ioctlDiskGetGeometryEx = 0x700A0
)

// OpenRawDevice opens `\\.\E:`, `\\.\PhysicalDriveN` or an image file for reading only
// (GENERIC_READ). Worker side only. Not elevated → ErrNeedsAdmin.
func OpenRawDevice(dev string) (io.ReaderAt, int64, io.Closer, error) {
	if !strings.HasPrefix(dev, `\\.\`) {
		return openImageFile(dev)
	}
	p, err := windows.UTF16PtrFromString(dev)
	if err != nil {
		return nil, 0, nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		if err == windows.ERROR_ACCESS_DENIED {
			err = os.ErrPermission
		}
		return nil, 0, nil, needsAdmin(&os.PathError{Op: "open", Path: dev, Err: err})
	}
	var n uint32
	length := make([]byte, 8)
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0, &length[0], 8, &n, nil); err != nil {
		windows.CloseHandle(h)
		return nil, 0, nil, &os.PathError{Op: "size", Path: dev, Err: err}
	}
	size := int64(binary.LittleEndian.Uint64(length))
	sector := int64(512)
	geo := make([]byte, 256)
	if windows.DeviceIoControl(h, ioctlDiskGetGeometryEx, nil, 0, &geo[0], uint32(len(geo)), &n, nil) == nil && n >= 24 {
		if s := int64(binary.LittleEndian.Uint32(geo[20:])); s >= 512 && s <= 65536 {
			sector = s
		}
	}
	f := os.NewFile(uintptr(h), dev)
	return NewAligned(f, sector, size), size, f, nil
}
