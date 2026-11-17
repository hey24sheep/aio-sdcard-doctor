package drives

import (
	"path/filepath"
	"testing"
)

// Read as a folder (Windows: "1234-ABCD", disk size) or directly (\\.\E:: raw serial, volume
// size), one card is one card: the same lock, and a resume matches.
func TestLockKeySameCardVolumeOrDisk(t *testing.T) {
	folder := Identity{FSSerial: "1234-ABCD", Size: 64 << 30}
	raw := Identity{FSSerial: "000000001234ABCD", Size: 63 << 30} // NTFS-style 64-bit serial, volume length
	if folder.LockKey() != raw.LockKey() {
		t.Fatal("folder and raw reads of one card get different locks")
	}
	if ok, _, _ := folder.Matches(raw); !ok {
		t.Fatal("raw resume of a folder rescue refused")
	}
	other := Identity{FSSerial: "9999-0000", Size: 64 << 30}
	if folder.LockKey() == other.LockKey() {
		t.Fatal("two cards share a lock")
	}
}

// A different card put into the same reader (same drive letter) is not the card being copied.
func TestFindCardFolderRejectsStranger(t *testing.T) {
	dir := t.TempDir()
	old := Lister
	defer func() { Lister = old }()
	Lister = func() ([]Drive, error) {
		return []Drive{{Root: filepath.VolumeName(dir) + `\`, ID: Identity{FSSerial: "OTHER-0001"}}}, nil
	}
	if _, ok := FindCard(Identity{FSSerial: "S1"}, dir); ok {
		t.Fatal("stranger card at the same path accepted")
	}
	if got, ok := FindCard(Identity{FSSerial: "OTHER-0001"}, dir); !ok || got != dir {
		t.Fatalf("the right card at the same path refused: %q %v", got, ok)
	}
}

// OA1: when only one of two identities had a file-system serial nothing was compared, and
// "nothing compared" counted as the same card — a worker respawn then read a stranger of the
// same size. With one serial the content samples decide, and at least one must be compared.
func TestMatchesNeedsSomethingInCommon(t *testing.T) {
	const size = 64 << 30
	samples := func(h string) []FPBlock {
		return []FPBlock{{Where: "off:0", SHA256: h}, {Where: "off:65536", SHA256: h + "2"}}
	}
	unreadable := Identity{Size: size, Fingerprint: samples("AA")}
	withSerial := Identity{Size: size, FSSerial: "1234-ABCD"}
	for name, c := range map[string]struct {
		a, b Identity
		want bool
	}{
		"unreadable vs a serial, no samples":      {unreadable, withSerial, false},
		"serial, samples agree":                   {Identity{Size: size, FSSerial: "1234-ABCD", Fingerprint: samples("AA")}, unreadable, true},
		"serial, samples differ":                  {Identity{Size: size, FSSerial: "1234-ABCD", Fingerprint: samples("BB")}, unreadable, false},
		"an empty identity vs a card":             {Identity{}, withSerial, false},
		"no serials, no samples readable":         {Identity{Size: size}, Identity{Size: size, Fingerprint: samples("")}, false},
		"no serials, samples agree":               {unreadable, unreadable, true},
		"samples only (imaging), none comparable": {Identity{}, Identity{}, true},
	} {
		if ok, _, _ := c.a.Matches(c.b); ok != c.want {
			t.Errorf("%s: %v", name, ok)
		}
		if ok, _, _ := c.b.Matches(c.a); ok != c.want {
			t.Errorf("%s (reversed): %v", name, ok)
		}
	}
}
