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
