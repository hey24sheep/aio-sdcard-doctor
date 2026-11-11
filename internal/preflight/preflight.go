// Package preflight checks an action before it starts and reports every problem as an Issue:
// Block stops it, Warn needs the user's OK, Info is shown. Space is computed up front.
package preflight

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
)

const (
	// Reserve is the free space always left on the destination.
	Reserve = 1 << 30
	// FATMaxFile is the largest file FAT/FAT32 can hold.
	FATMaxFile = 4<<30 - 1
	systemLow  = 20 << 30
)

// Input is everything the checks look at.
type Input struct {
	Action     string // "rescue" | "image" | "recover" | "verify"
	Src        drives.Drive
	SrcDevice  *drives.Device
	Dst        drives.Drive
	DstPath    string
	Items      []scan.Item // the files the user selected
	Suggested  map[string]scan.Group
	Already    func(src string) (manifest.Entry, bool)
	Admin      bool
	NeedsAdmin bool
	DeviceSize int64 // image: bytes to write
	Resume     *job.ResumeInfo
}

var stopOnly = []job.Choice{job.Stop}
var warnChoices = []job.Choice{job.Continue, job.Stop}

func block(kind, title, detail string) job.Issue {
	return job.Issue{Kind: kind, Title: title, Detail: detail, Severity: job.Block, Choices: stopOnly, Default: job.Stop}
}

func warn(kind, title, detail string) job.Issue {
	return job.Issue{Kind: kind, Title: title, Detail: detail, Severity: job.Warn, Choices: warnChoices, Default: job.Continue}
}

// GB formats a byte count for people ("1.5 GB").
func GB(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// Commas formats 9624 as "9,624".
func Commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// IsFAT reports whether a file-system name is FAT as Windows, Linux ("vfat") or macOS ("msdos")
// names it.
func IsFAT(fsName string) bool {
	switch strings.ToUpper(fsName) {
	case "FAT", "FAT12", "FAT16", "FAT32", "VFAT", "MSDOS":
		return true
	}
	return false
}

// Under reports whether p is root or inside it (case-insensitive on Windows and macOS).
func Under(p, root string) bool { return under(p, root) }

func under(p, root string) bool {
	if root == "" || p == "" {
		return false
	}
	p, root = filepath.Clean(p), filepath.Clean(root)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, `\/`)+string(filepath.Separator))
}

// Ours reports whether dir is empty, missing, or a folder this program wrote (Reports/state.db).
func Ours(dir string) bool {
	es, err := os.ReadDir(dir)
	if err != nil || len(es) == 0 {
		return true
	}
	if len(es) == 1 && es[0].IsDir() && es[0].Name() == "Reports" {
		if rs, err := os.ReadDir(filepath.Join(dir, "Reports")); err == nil && len(rs) == 0 {
			return true // only the empty Reports folder our own write probe made
		}
	}
	for _, db := range []string{"state.db", "recover.db"} { // a rescue or a recover
		if _, err := os.Stat(filepath.Join(dir, "Reports", db)); err == nil {
			return true
		}
	}
	return false
}

// writable probes the destination by creating and removing Reports/.probe.
func writable(dir string) error {
	// Folders the probe has to create are removed again, so a cancelled run leaves nothing.
	base := filepath.Clean(dir)
	var made []string
	for i := 0; i < 256; i++ {
		if _, err := os.Stat(base); err == nil || filepath.Dir(base) == base {
			break
		}
		made = append(made, base)
		base = filepath.Dir(base)
	}
	out := destfs.New(base)
	reports := filepath.Join(dir, "Reports")
	if _, err := os.Stat(reports); err != nil {
		made = append([]string{reports}, made...)
	}
	defer func() {
		for _, d := range made { // deepest first; only empty folders go
			out.Remove(d)
		}
	}()
	if err := out.MkdirAll(reports); err != nil {
		return err
	}
	probe := filepath.Join(reports, ".probe")
	if err := out.Remove(probe); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := out.Create(probe)
	if err != nil {
		return err
	}
	f.Close()
	return out.Remove(probe)
}

// Check runs every pre-flight rule.
func Check(in Input) []job.Issue {
	var is []job.Issue
	srcPhys := in.Src.PhysicalID
	if in.SrcDevice != nil && in.SrcDevice.PhysicalID != "" {
		srcPhys = in.SrcDevice.PhysicalID
	}
	destOK := true
	// where the destination really is: a junction, subst letter, folder mount point or symlink
	// can put a harmless-looking path on the card itself
	onCard := in.Src.Root != "" && (under(in.DstPath, in.Src.Root) || under(drives.RealPath(in.DstPath), drives.RealPath(in.Src.Root)))
	if s := drives.NormalizeSerial(in.Src.ID.FSSerial); !onCard && s != "" && (in.Src.Removable || in.SrcDevice != nil) {
		onCard = drives.VolumeSerial(in.DstPath) == s // the destination's own volume is the card's
	}
	switch {
	case onCard:
		is = append(is, block("dest-inside-source", "The destination is on the card",
			"Choose a folder on another disk — the card is only ever read."))
		destOK = false
	case srcPhys != "" && in.Dst.PhysicalID == srcPhys && (in.Src.Removable || in.SrcDevice != nil):
		is = append(is, block("same-disk", "The destination is on the same physical disk as the card",
			"Choose a folder on another disk."))
		destOK = false
	case srcPhys != "" && in.Dst.PhysicalID == srcPhys:
		is = append(is, warn("same-disk-internal", "Source and destination are on the same physical disk",
			"Fine for copying between internal drives. If that disk is failing, copy to another disk instead."))
	}
	if in.Src.Locked {
		is = append(is, block("source-locked", "The card is locked (encrypted)",
			"unlock it in Explorer/Disk Utility, then retry."))
	}
	if destOK && in.Action != "verify" && in.Action != "image" && !Ours(in.DstPath) { // an image never overwrites (O_EXCL)
		is = append(is, block("dest-foreign", "The destination folder already holds other files",
			"Choose an empty folder or a previous rescue folder, so nothing is overwritten."))
		destOK = false
	}
	if destOK && in.Action != "verify" {
		if in.Dst.WriteProtected {
			is = append(is, block("dest-not-writable", "The destination is write-protected", "Choose another destination."))
		} else if err := writable(in.DstPath); err != nil {
			is = append(is, block("dest-not-writable", "Can't write to the destination", err.Error()))
		}
	}
	if in.Action == "image" {
		if int64(in.Dst.Free) < in.DeviceSize+Reserve {
			is = append(is, block("no-space-image", "Not enough space for the card image",
				fmt.Sprintf("Needs %s free (card size + 1 GB), %s available.", GB(in.DeviceSize+Reserve), GB(int64(in.Dst.Free)))))
		}
		if IsFAT(in.Dst.FS) && in.DeviceSize > FATMaxFile {
			is = append(is, block("fat-image", "The destination can't hold a file this large",
				"FAT32 files stop at 4 GB — choose an NTFS/exFAT/APFS/ext4 destination."))
		}
	}
	var need int64
	var big []string
	for _, it := range in.Items {
		if in.Already != nil {
			if e, ok := in.Already(it.Rel); ok && (e.Status == manifest.OK || e.Status == manifest.ALREADY_COPIED) {
				continue
			}
		}
		need += it.Size
		if it.Size > FATMaxFile {
			big = append(big, it.Rel)
		}
	}
	if in.Action != "image" && in.Action != "verify" && need > 0 {
		if need+Reserve > int64(in.Dst.Free) {
			short := need + Reserve - int64(in.Dst.Free)
			is = append(is, warn("short-space", fmt.Sprintf("The destination is short by %s", GB(short)),
				fmt.Sprintf("Needs %s, %s free — the copy stops safely when full.", GB(need+Reserve), GB(int64(in.Dst.Free)))))
		}
		if IsFAT(in.Dst.FS) && len(big) > 0 {
			sort.Strings(big)
			is = append(is, warn("fat-big-files", fmt.Sprintf("%d files are too big for a FAT32 destination", len(big)),
				"They will be reported as FAILED: "+strings.Join(big[:min(10, len(big))], ", ")))
		}
		if in.Dst.System && int64(in.Dst.Free)-need < systemLow {
			is = append(is, warn("system-drive-low", "Your system disk will be nearly full",
				fmt.Sprintf("Less than %s would be left on %s.", GB(systemLow), in.Dst.Root)))
		}
	}
	if !in.Src.Removable && in.Src.Root != "" && in.Action != "verify" {
		is = append(is, warn("not-removable", "The source is an internal drive, not a card or USB drive",
			fmt.Sprintf("%s is fine to copy from if that's what you meant; it is only read.", in.Src.Root)))
	}
	if len(in.Suggested) > 0 {
		var parts []string
		for id, g := range in.Suggested {
			parts = append(parts, fmt.Sprintf("%s: %d files (%s)", id, g.Files, GB(g.Bytes)))
		}
		sort.Strings(parts)
		is = append(is, job.Issue{Kind: "suggestions", Title: "Some files could be skipped — you decide",
			Detail: strings.Join(parts, "; "), Severity: job.Info, Choices: []job.Choice{job.Continue}, Default: job.Continue})
	}
	if in.Src.RAW {
		is = append(is, job.Issue{Kind: "raw-source", Title: "The OS can't read this card's file system",
			Detail: "Reading it with the built-in parsers.", Severity: job.Info, Choices: []job.Choice{job.Continue}, Default: job.Continue})
	}
	if in.NeedsAdmin && !in.Admin {
		is = append(is, job.Issue{Kind: "needs-admin", Title: "Reading the card directly needs administrator rights",
			Severity: job.Info, Choices: []job.Choice{job.Elevate, job.Stop}, Default: job.Elevate})
	}
	if in.Resume != nil {
		is = append(is, job.Issue{Kind: "resume",
			Title:    fmt.Sprintf("A previous run stopped: %s of %s done", Commas(in.Resume.Done), Commas(in.Resume.Total)),
			Detail:   "Resume continues where it stopped; start over re-checks every file.",
			Severity: job.Info, Choices: []job.Choice{job.Resume, job.New}, Default: job.Resume})
	}
	return is
}
