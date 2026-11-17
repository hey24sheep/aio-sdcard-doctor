package fix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// VerifiedRescue reports whether manifestPath (a rescue's Reports/manifest.json) is a rescue of
// this whole card whose last verify said ALL FILES MATCH; why says what is missing when not.
// Every front end asks the same question, so the rule lives here.
func VerifiedRescue(manifestPath string, t Target) (bool, string) {
	if manifestPath == "" {
		return false, ""
	}
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, "can't read " + manifestPath
	}
	var m struct {
		Volume     drives.Identity
		SourceKind string
		SourceRoot string
		Entries    []struct{ Status string }
	}
	if json.Unmarshal(b, &m) != nil {
		return false, manifestPath + " is not a rescue manifest"
	}
	// a recovery of deleted files copies what was deleted, not what is on the card
	if m.SourceKind == "recover" {
		return false, "that is a recovery of deleted files, not a rescue of the card — rescue the card and verify it"
	}
	left, excluded := 0, 0
	for _, e := range m.Entries {
		switch e.Status {
		case "FAILED", "PARTIAL", "STALLED", "LOCKED", "UNREADABLE":
			left++
		case "EXCLUDED": // left out by the backup chosen (Photos only, …): never copied
			excluded++
		}
	}
	if left > 0 {
		return false, fmt.Sprintf("that rescue couldn't read %d files or folders — they may still be only on the card", left)
	}
	if excluded > 0 {
		return false, fmt.Sprintf("that rescue left out %d files (not part of the backup chosen) — they are still only on the card", excluded)
	}
	if t.Vol.ID.FSSerial == "" && len(t.Vol.ID.Fingerprint) == 0 {
		return false, "this card has no readable identity (no file-system serial), so no rescue can be matched to it"
	}
	// a rescue of one folder (rescue --from E:\DCIM) carries the card's identity too, yet the rest
	// of the card was never copied: only a rescue of the card itself counts
	if m.SourceRoot == "" {
		return false, "that rescue doesn't say what it copied (made by an older version) — rescue the card again and verify it"
	}
	if !wholeCard(m.SourceRoot, t) {
		return false, fmt.Sprintf("that rescue copied %s, not the whole card (%s) — rescue the whole card and verify it",
			m.SourceRoot, nonEmpty(t.Vol.Root, nonEmpty(t.Vol.Device, t.Disk.Path)))
	}
	if ok, _, _ := m.Volume.Matches(t.Vol.ID); !ok {
		return false, "that rescue is of a different card"
	}
	sums, _ := filepath.Glob(filepath.Join(filepath.Dir(manifestPath), "verify_summary_*.txt"))
	sort.Strings(sums)
	if len(sums) == 0 {
		return false, "that rescue was never verified — run verify first"
	}
	if s, _ := os.ReadFile(sums[len(sums)-1]); !strings.Contains(string(s), "ALL FILES MATCH") {
		return false, "the last verify of that rescue found problems"
	}
	return true, ""
}

// wholeCard: src (a rescue's source as given) is the card's volume root, its volume device or
// its whole disk — not a folder on it, nor an image file.
func wholeCard(src string, t Target) bool {
	norm := func(s string) string {
		if strings.HasPrefix(s, `\\.\`) {
			return strings.ToLower(s)
		}
		s = strings.TrimRight(drives.NormalizeRoot(s), `\/`) // "E:", "E:\" and "e:/" are one root
		s = strings.Replace(s, "/dev/rdisk", "/dev/disk", 1) // macOS raw node of the same disk
		return strings.ToLower(s)
	}
	for _, p := range []string{t.Vol.Root, t.Vol.Device, t.Disk.Path} {
		if p != "" && norm(src) == norm(p) && norm(p) != "" {
			return true
		}
	}
	return false
}

// FingerprintPath is the device whose first readable block tells this card from another of the
// same size put in the same reader, read before anything is written (DiskFingerprint): "" when
// the card's file-system serial does that. A RAW volume or a card with no partition has none
// (OA2), and a reader often reports its own serial, not the card's.
func FingerprintPath(t Target) string {
	if drives.NormalizeSerial(t.Vol.ID.FSSerial) != "" {
		return ""
	}
	if t.Vol.Device != "" {
		return t.Vol.Device
	}
	return t.Disk.Path
}
