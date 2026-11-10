package source

import (
	"io"
	"os"
	"strings"

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
	// libSystem's ioctl (direct syscalls are deprecated on macOS). IoctlGetInt reads into a
	// zeroed 64-bit int: block size is a uint32 (little-endian, so the low half), block count a
	// uint64; every Mac Go supports is 64-bit little-endian.
	bs, err := unix.IoctlGetInt(int(f.Fd()), dkiocGetBlockSize)
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	bc, err := unix.IoctlGetInt(int(f.Fd()), dkiocGetBlockCount)
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	size := int64(bs) * int64(bc)
	return NewAligned(f, int64(bs), size), size, f, nil
}
