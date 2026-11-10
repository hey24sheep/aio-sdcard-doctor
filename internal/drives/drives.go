// Package drives lists volumes and physical disks with what the safety checks need: removable,
// system, locked, write-protected, RAW, physical-disk id and a card identity that survives a
// re-plug. Nothing here reads card data.
package drives

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// FPBlock is one fingerprint sample: a hash of data already read from the card.
type FPBlock struct {
	Where  string // "off:<n>" (raw) or "file:<rel>" (folder)
	SHA256 string // "" = unreadable this time
}

// Identity tells one card from another (R2).
type Identity struct {
	PhysicalID, DeviceSerial, Model string // cheap, no reads
	Size                            uint64
	FSSerial, Label                 string    // OS volume serial/UUID; boot sector when raw
	Fingerprint                     []FPBlock // hashes of already-read data
}

// NormalizeSerial makes file-system serials comparable however they were read: Windows reports
// "1234-ABCD", a raw FAT/exFAT boot sector the same 32 bits, and a raw NTFS boot sector a 64-bit
// value whose low 32 bits are what Windows shows. Hex digits only, uppercase, the last 8.
func NormalizeSerial(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'F') {
			b.WriteRune(r)
		}
	}
	h := b.String()
	if len(h) > 8 {
		h = h[len(h)-8:]
	}
	return h
}

// LockKey is the per-card lock key (R14): the card's file-system serial, which is the same
// whether the card is read as a folder, as a volume or as a whole device, after a re-plug and
// in another reader. Without a serial: size and physical disk.
func (a Identity) LockKey() string {
	k := "fs:" + NormalizeSerial(a.FSSerial)
	if NormalizeSerial(a.FSSerial) == "" {
		k = fmt.Sprintf("disk:%d|%s", a.Size, a.PhysicalID)
	}
	s := sha256.Sum256([]byte(k))
	return hex.EncodeToString(s[:])
}

// Matches reports whether b is the same card as a. When both know the file-system serial it
// decides (sizes differ between a volume and its disk); otherwise sizes must agree. Model and
// DeviceSerial are ignored because on USB readers they describe the reader. Fingerprint blocks
// unreadable on either side are skipped; at least 3 must match (or all, when fewer can be
// compared) and none may differ.
func (a Identity) Matches(b Identity) (ok bool, matched, compared int) {
	sa, sb := NormalizeSerial(a.FSSerial), NormalizeSerial(b.FSSerial)
	switch {
	case sa != "" && sb != "":
		if sa != sb {
			return false, 0, 0
		}
	case a.Size != 0 && b.Size != 0 && a.Size != b.Size:
		return false, 0, 0
	}
	theirs := map[string]string{}
	for _, f := range b.Fingerprint {
		theirs[f.Where] = f.SHA256
	}
	for _, f := range a.Fingerprint {
		h, ok := theirs[f.Where]
		if !ok || h == "" || f.SHA256 == "" {
			continue
		}
		compared++
		if h != f.SHA256 {
			return false, matched, compared
		}
		matched++
	}
	return matched >= min(3, compared), matched, compared
}

// Drive is a mounted volume (or a drive letter whose file system the OS can't read).
type Drive struct {
	Root, Label, FS string
	Total, Free     uint64
	Removable       bool
	System          bool
	Locked          bool // BitLocker/FileVault locked or otherwise unreadable
	WriteProtected  bool
	RAW             bool // the OS doesn't recognise the file system
	ID              Identity
	PhysicalID      string
	Device          string // the volume's raw device (\\.\E:, /dev/sdb1, /dev/disk4s1)
}

// Device is a physical disk, mounted or not.
type Device struct {
	Path, Model, Serial, Bus string
	Size                     int64
	Removable, Mounted       bool
	ReadOnly                 bool
	PhysicalID               string
	Volumes                  []Drive
	InUse                    string // why the OS holds the whole disk (LVM, RAID, dm-crypt member); "" if not
}

// Lister and DeviceLister are seams for tests.
var (
	Lister       = List
	DeviceLister = ListDevices
)

// NormalizeRoot turns "E", "e:", "E:" and "E:\" into the drive root "E:\" ("E:" alone means
// "current folder on E:"), and strips trailing separators from folders.
func NormalizeRoot(s string) string {
	if runtime.GOOS == "windows" {
		t := strings.TrimRight(s, `\/`)
		if len(t) == 1 && isLetter(t[0]) {
			t += ":"
		}
		if len(t) == 2 && t[1] == ':' && isLetter(t[0]) {
			return strings.ToUpper(t[:1]) + `:\`
		}
		return filepath.Clean(t)
	}
	if s == "/" {
		return s
	}
	return filepath.Clean(strings.TrimRight(s, "/"))
}

func isLetter(c byte) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }

func under(p, root string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	root = strings.TrimRight(root, `\/`)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator)) || (root == "" && strings.HasPrefix(p, "/"))
}

// Lookup returns the volume holding path (longest matching root).
func Lookup(path string) (Drive, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Drive{}, err
	}
	ds, err := Lister()
	if err != nil {
		return Drive{}, err
	}
	if m := mountOf(abs); m != "" {
		for _, d := range ds {
			if d.Root == m {
				return d, nil
			}
		}
	}
	best := -1
	for i, d := range ds {
		if under(abs, d.Root) && (best < 0 || len(d.Root) > len(ds[best].Root)) {
			best = i
		}
	}
	if best < 0 {
		return Drive{}, fmt.Errorf("no volume found for %s", path)
	}
	return ds[best], nil
}

var diskPart = regexp.MustCompile(`^(r?disk\d+)s\d+$`)

// physicalOfPath parses a whole-device path into a physical-disk id without system calls.
func physicalOfPath(p string) string {
	switch {
	case strings.HasPrefix(p, `\\.\PhysicalDrive`):
		return strings.TrimPrefix(p, `\\.\`)
	case strings.HasPrefix(p, "/dev/"):
		name := strings.TrimPrefix(p, "/dev/")
		if m := diskPart.FindStringSubmatch(name); m != nil {
			name = m[1]
		}
		return strings.TrimPrefix(name, "r") // macOS raw node rdiskN → diskN
	}
	return ""
}

// PhysicalOf returns the physical-disk id of a device or volume path (\\.\PhysicalDriveN,
// \\.\E:, /dev/sdb1, /dev/disk4s1), or "" for files and folders.
func PhysicalOf(p string) string {
	if strings.HasPrefix(p, `\\.\`) && len(p) == 6 && p[5] == ':' { // \\.\E:
		if d, err := Lookup(p[4:6] + `\`); err == nil {
			return d.PhysicalID
		}
		return ""
	}
	id := physicalOfPath(p)
	if runtime.GOOS == "linux" && id != "" {
		if dev := partitionParent(id); dev != "" {
			return dev
		}
	}
	return id
}
