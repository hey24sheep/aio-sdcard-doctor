package source

import (
	"io"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	dkiocGetBlockSize  = 0x40046418
	dkiocGetBlockCount = 0x40086419
)

// OpenRawDevice opens /dev/diskN as the unbuffered /dev/rdiskN (or an image file) read-only.
// Worker side only.
func OpenRawDevice(dev string) (io.ReaderAt, int64, io.Closer, error) {
	if !strings.HasPrefix(dev, "/dev/") {
		return openImageFile(dev)
	}
	if strings.HasPrefix(dev, "/dev/disk") {
		dev = "/dev/r" + strings.TrimPrefix(dev, "/dev/")
	}
	f, err := os.Open(dev)
	if err != nil {
		return nil, 0, nil, needsAdmin(err)
	}
	var bs uint32
	var bc uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), dkiocGetBlockSize, uintptr(unsafe.Pointer(&bs))); e != 0 {
		f.Close()
		return nil, 0, nil, e
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), dkiocGetBlockCount, uintptr(unsafe.Pointer(&bc))); e != 0 {
		f.Close()
		return nil, 0, nil, e
	}
	size := int64(bs) * int64(bc)
	return NewAligned(f, int64(bs), size), size, f, nil
}
