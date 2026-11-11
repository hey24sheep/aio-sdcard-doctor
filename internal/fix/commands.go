package fix

import (
	"fmt"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// fat32Max: a card up to 32 GiB is formatted FAT32 (what cameras expect); larger ones exFAT.
const fat32Max = 32 << 30

// family normalises a file-system name: fat, exfat, ntfs, ext4, apfs, hfs+, raw.
func family(fs string) string {
	switch f := strings.ToLower(fs); {
	case strings.HasPrefix(f, "fat"), f == "vfat", f == "msdos":
		return "fat"
	case f == "exfat":
		return "exfat"
	case f == "ntfs":
		return "ntfs"
	case strings.HasPrefix(f, "ext"):
		return "ext4"
	case f == "apfs":
		return "apfs"
	case f == "hfs+", f == "hfs":
		return "hfs+"
	}
	return "raw"
}

// auto: an unmounted Linux volume whose type isn't known without probing; fsck detects it.
func auto(goos string, t Target) bool { return goos == "linux" && t.Vol.FS == "" && t.Vol.Device != "" }

// newFS is the file system Format makes on the target.
func newFS(t Target) string {
	if t.Size() <= fat32Max {
		return "fat32"
	}
	return "exfat"
}

// cleanLabel keeps what FAT/exFAT labels allow (FAT: 11 characters, upper case).
func cleanLabel(l string, fs string) string {
	var b strings.Builder
	for _, r := range l {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	max := 15
	if fs == "fat32" {
		s, max = strings.ToUpper(s), 11
	}
	if len(s) > max {
		s = strings.TrimSpace(s[:max])
	}
	if s == "" {
		s = "CARD"
	}
	return s
}

// Command is the OS tool line for check, repair or format (the OS's own tools only; our code
// never writes file-system metadata). newLabel "" keeps the card's label.
func Command(goos, op string, t Target, newLabel string) ([]string, error) {
	fam := family(t.Vol.FS)
	label := newLabel
	if label == "" {
		label = t.Vol.Label
	}
	nfs := newFS(t)
	label = cleanLabel(label, nfs)
	switch goos {
	case "windows":
		letter := strings.TrimRight(t.Vol.Root, `\`)
		if len(letter) != 2 || letter[1] != ':' {
			return nil, fmt.Errorf("the card has no drive letter: Windows' tools need one (give it a letter in Disk Management)")
		}
		switch op {
		case OpCheck:
			return []string{"chkdsk", letter}, nil
		case OpRepair:
			return []string{"chkdsk", letter, "/f", "/x"}, nil
		case OpFormat:
			return []string{"format", letter, "/FS:" + map[string]string{"fat32": "FAT32", "exfat": "exFAT"}[nfs], "/Q", "/V:" + label, "/Y"}, nil
		}
	case "darwin":
		dev := t.Vol.Device
		if dev == "" {
			return nil, fmt.Errorf("no device found for the card")
		}
		switch op {
		case OpCheck:
			return []string{"diskutil", "verifyVolume", dev}, nil
		case OpRepair:
			return []string{"diskutil", "repairVolume", dev}, nil
		case OpFormat:
			return []string{"diskutil", "eraseVolume", map[string]string{"fat32": "FAT32", "exfat": "ExFAT"}[nfs], label, dev}, nil
		}
	case "linux":
		dev := t.Vol.Device
		if dev == "" {
			return nil, fmt.Errorf("no device found for the card")
		}
		if op != OpCheck && t.Vol.Root != "" {
			return nil, fmt.Errorf("unmount the card first (umount %s), then run this again", t.Vol.Root)
		}
		switch op {
		case OpCheck, OpRepair:
			if auto(goos, t) {
				if op == OpRepair {
					return []string{"fsck", "-a", dev}, nil
				}
				return []string{"fsck", "-n", dev}, nil
			}
			flag := map[string][2]string{"fat": {"-n", "-a"}, "exfat": {"-n", "-y"}, "ext4": {"-n", "-p"}}[fam]
			tool := map[string]string{"fat": "fsck.vfat", "exfat": "fsck.exfat", "ext4": "e2fsck"}[fam]
			if tool == "" {
				return nil, fmt.Errorf("no %s check tool on Linux", fam)
			}
			f := flag[0]
			if op == OpRepair {
				f = flag[1]
			}
			return []string{tool, f, dev}, nil
		case OpFormat:
			if nfs == "fat32" {
				return []string{"mkfs.vfat", "-F", "32", "-n", label, dev}, nil
			}
			return []string{"mkfs.exfat", "-n", label, dev}, nil
		}
	}
	return nil, fmt.Errorf("%s isn't available on %s", op, goos)
}

func capOf(name string, ok bool, why string) job.Capability {
	if ok {
		why = ""
	}
	return job.Capability{Name: name, Enabled: ok, Reason: why}
}

// Available says which operations work for this card on this OS, each with its reason when not.
// Order: Check, Repair, Format, Health test.
// FormatElsewhere says where to format a card that has no partition (after a health test).
func FormatElsewhere(goos string) string {
	switch goos {
	case "windows":
		return "format it in Disk Management (right-click the card, New Simple Volume)"
	case "darwin":
		return "format it in Disk Utility (Erase, MS-DOS (FAT) or ExFAT)"
	}
	return "format it with GParted, or create a partition and run mkfs.exfat"
}

func Available(goos string, t Target, fsKind string) []job.Capability {
	fam := family(fsKind)
	if fsKind == "" {
		fam = family(t.Vol.FS)
	}
	check, why := true, ""
	switch {
	case fsKind == "" && auto(goos, t):
		if _, err := lookPath("fsck"); err != nil {
			check, why = false, "fsck not installed (util-linux)"
		}
	case fam == "raw":
		check, why = false, "the file system isn't recognised — Rescue or Format"
	case goos == "windows" && (fam == "ext4" || fam == "apfs" || fam == "hfs+"):
		check, why = false, "no "+strings.ToUpper(fam)+" check tool on Windows"
	case goos == "darwin" && fam == "ntfs":
		check, why = false, "no NTFS repair tool on macOS"
	case goos == "darwin" && (fam == "apfs" || fam == "hfs+"):
		check, why = false, "HFS+/APFS: use Disk Utility"
	case goos == "darwin" && fam == "ext4":
		check, why = false, "no ext4 check tool on macOS"
	case goos == "linux" && fam == "ntfs":
		check, why = false, "no NTFS repair tool on Linux — use Windows chkdsk"
	case goos == "linux" && (fam == "apfs" || fam == "hfs+"):
		check, why = false, "HFS+/APFS: check it on a Mac"
	case goos == "linux":
		tool, pkg := map[string][2]string{"fat": {"fsck.vfat", "dosfstools"}, "exfat": {"fsck.exfat", "exfatprogs"},
			"ext4": {"e2fsck", "e2fsprogs"}}[fam][0], map[string]string{"fat": "dosfstools", "exfat": "exfatprogs", "ext4": "e2fsprogs"}[fam]
		if _, err := lookPath(tool); err != nil {
			check, why = false, tool+" not installed ("+pkg+")"
		}
	}
	format, fwhy := true, ""
	if goos == "linux" {
		tool, pkg := "mkfs.vfat", "dosfstools"
		if newFS(t) == "exfat" {
			tool, pkg = "mkfs.exfat", "exfatprogs"
		}
		if _, err := lookPath(tool); err != nil {
			format, fwhy = false, tool+" not installed ("+pkg+")"
		}
	}
	health, hwhy := true, ""
	repair, rwhy := check, why
	// a card left without a partition (after a health test) has no volume: nothing this tool can
	// check, repair or format until the OS's own disk tool makes one
	if t.Vol.Root == "" && t.Vol.Device == "" {
		const none = "the card has no partition (as a health test leaves it)"
		check, why, repair, rwhy = false, none, false, none
		format, fwhy = false, none+" — "+FormatElsewhere(goos)
	}
	if t.Disk.ReadOnly || t.Vol.WriteProtected {
		const lock = "card is write-protected — slide the lock switch"
		repair, rwhy, format, fwhy, health, hwhy = false, lock, false, lock, false, lock
	}
	return []job.Capability{capOf("Check", check, why), capOf("Repair", repair, rwhy), capOf("Format", format, fwhy),
		capOf("Health test", health, hwhy)}
}
