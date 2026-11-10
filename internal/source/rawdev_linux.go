package source

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenRawDevice opens /dev/sdX, /dev/mmcblkN (or an image file) read-only. Worker side only.
func OpenRawDevice(dev string) (io.ReaderAt, int64, io.Closer, error) {
	if !strings.HasPrefix(dev, "/dev/") {
		return openImageFile(dev)
	}
	f, err := os.Open(dev)
	if err != nil {
		return nil, 0, nil, needsAdmin(err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	sector := int64(512)
	if s, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKSSZGET); err == nil && s >= 512 {
		sector = int64(s)
	}
	return NewAligned(f, sector, size), size, f, nil
}
