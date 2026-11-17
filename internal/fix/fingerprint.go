package fix

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// DiskFingerprint hashes the first readable 4 KiB of a whole disk, read-only: a card with no
// partition has no label or serial, and a reader often reports its own serial, so this is what
// tells it from another card of the same size put in the same reader. "" when nothing can be
// read (no rights, no card). Callers keep it behind a seam: tests never open a device.
func DiskFingerprint(dev string) string {
	r, size, c, err := source.OpenRawDevice(dev)
	if err != nil {
		return ""
	}
	defer c.Close()
	b := make([]byte, 4096)
	for off := int64(0); off < min(size, 64<<10); off += int64(len(b)) {
		if n, err := r.ReadAt(b, off); n == len(b) && (err == nil || errors.Is(err, io.EOF)) {
			return fmt.Sprintf("%d:%x", off, sha256.Sum256(b))
		}
	}
	return ""
}
