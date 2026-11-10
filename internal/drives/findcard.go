package drives

import (
	"os"
	"strings"
)

// FindCard finds a card again after it was removed (or checks it is still there):
//   - a volume read directly (\\.\E:) by its file-system serial, possibly under another letter;
//   - a whole device (\\.\PhysicalDriveN, /dev/sdX) at the same path with the same size, or by
//     its serial number — never another disk that merely has the same size;
//   - a folder, mount point or image file by path, else a mounted volume with the same
//     file-system serial and size.
func FindCard(id Identity, from string) (string, bool) {
	switch {
	case strings.HasPrefix(from, `\\.\`) && len(from) == 6 && from[5] == ':':
		return findVolume(id)
	case strings.HasPrefix(from, `\\.\`) || strings.HasPrefix(from, "/dev/"):
		devs, err := DeviceLister()
		if err != nil {
			return "", false
		}
		for _, d := range devs {
			if d.Path == from && (id.Size == 0 || uint64(d.Size) == id.Size) {
				return from, true
			}
		}
		if id.DeviceSerial != "" {
			for _, d := range devs {
				if d.Serial == id.DeviceSerial && uint64(d.Size) == id.Size {
					return d.Path, true
				}
			}
		}
		return "", false
	}
	if _, err := os.Stat(from); err == nil {
		// The path is there — but is it the same card? Another card put into the same reader gets
		// the same drive letter.
		d, lerr := Lookup(from)
		if lerr != nil || id.FSSerial == "" || d.ID.FSSerial == "" ||
			NormalizeSerial(d.ID.FSSerial) == NormalizeSerial(id.FSSerial) {
			return from, true
		}
	}
	if id.FSSerial == "" {
		return "", false
	}
	ds, err := Lister()
	if err != nil {
		return "", false
	}
	for _, d := range ds {
		if NormalizeSerial(d.ID.FSSerial) == NormalizeSerial(id.FSSerial) {
			return d.Root, true
		}
	}
	return "", false
}

func findVolume(id Identity) (string, bool) {
	if id.FSSerial == "" {
		return "", false
	}
	ds, err := Lister()
	if err != nil {
		return "", false
	}
	for _, d := range ds {
		if NormalizeSerial(d.ID.FSSerial) == NormalizeSerial(id.FSSerial) && len(d.Root) >= 2 {
			return `\\.\` + d.Root[:2], true
		}
	}
	return "", false
}
