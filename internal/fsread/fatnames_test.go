package fsread

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

// patchLFN rewrites a long name in a FAT image, UTF-16 unit for unit (same length, so the
// entry layout stays; the checksum covers only the short name).
func patchLFN(t *testing.T, p, from string, to []uint16) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := utf16.Encode([]rune(from))
	if len(want) != len(to) || len(want) > len(lfnSlots) {
		t.Fatal("patchLFN: one entry, same length only")
	}
	for off := 0; off+32 <= len(b); off += 32 {
		e := b[off : off+32]
		if e[11] != 0x0F || e[0]&0x3F != 1 {
			continue
		}
		match := true
		for i, u := range want {
			if uint16(e[lfnSlots[i]])|uint16(e[lfnSlots[i]+1])<<8 != u {
				match = false
				break
			}
		}
		if match {
			for i, u := range to {
				e[lfnSlots[i]], e[lfnSlots[i]+1] = byte(u), byte(u>>8)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("long name %q not found", from)
}

// Phones write emoji in FAT long names as UTF-16 surrogate pairs. go-diskfs decodes each unit
// on its own, so such a folder used to come back as "emoji��dir1" and could not be
// opened by its real name.
func TestFATNameOutsideBMP(t *testing.T) {
	p := image(t, "fat32", 64<<20, map[string][]byte{"emojiXYdir1/smile.txt": fill(100, "emoji")})
	real := "emoji\U0001F600dir1"
	patchLFN(t, p, "emojiXYdir1", utf16.Encode([]rune(real)))
	v, r := probeOne(t, p)
	src, err := Open(r, v)
	if err != nil {
		t.Fatal(err)
	}
	es, err := src.(fs.ReadDirFS).ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		found = found || e.Name() == real
	}
	if !found {
		names := []string{}
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Fatalf("root lists %q, want %q", names, real)
	}
	ra, size, c, err := src.OpenAt(real + "/smile.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make([]byte, size)
	ra.ReadAt(got, 0)
	if !bytes.Equal(got, fill(100, "emoji")) {
		t.Fatal("content differs")
	}
}

// Adversarial pass S4: two names that differ only in an emoji have the same go-diskfs spelling.
// Both were listed under one name and opened the same file: one was lost, the other got the
// wrong data. Each is listed and read as itself.
func TestFATEmojiNamesDontCollide(t *testing.T) {
	a, b := fill(100, "AAAA"), fill(200, "BBBB")
	p := image(t, "fat32", 64<<20, map[string][]byte{"partyAB.txt": a, "partyCD.txt": b})
	party1, party2 := "party\U0001F389.txt", "party\U0001F382.txt"
	patchLFN(t, p, "partyAB.txt", utf16.Encode([]rune(party1)))
	patchLFN(t, p, "partyCD.txt", utf16.Encode([]rune(party2)))
	v, r := probeOne(t, p)
	src, err := Open(r, v)
	if err != nil {
		t.Fatal(err)
	}
	es, _ := src.(fs.ReadDirFS).ReadDir(".")
	names := map[string]bool{}
	for _, e := range es {
		names[e.Name()] = true
	}
	for name, want := range map[string][]byte{party1: a, party2: b} {
		if !names[name] {
			t.Errorf("%q not listed: %v", name, names)
			continue
		}
		ra, size, c, err := src.OpenAt(name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		got := make([]byte, size)
		ra.ReadAt(got, 0)
		c.Close()
		if !bytes.Equal(got, want) {
			t.Errorf("%q: wrong contents (%d bytes)", name, size)
		}
	}
}

// Adversarial pass D5: a deleted file whose last long-name slot was reused by a new entry kept
// only its first 13 characters — "IMG_20230101_", no extension — so it was never checked and
// written as recovered. A long name that doesn't end with the short name's extension isn't
// trusted: the short name is used.
func TestDeletedLongNameMissingItsTail(t *testing.T) {
	short := []byte("IMG_20~1JPG")
	sum := shortSum(short)
	rec := func() []byte { return make([]byte, 32) }
	reused := rec() // the slot that held the name's tail, now a new file's entry
	copy(reused, "NEWFILE TXT")
	reused[11] = 0x20
	slot := rec() // the surviving first slot: "IMG_20230101_"
	slot[0], slot[11], slot[13] = 0xE5, 0x0F, sum
	for i, c := range utf16.Encode([]rune("IMG_20230101_")) {
		slot[lfnSlots[i]], slot[lfnSlots[i]+1] = byte(c), byte(c>>8)
	}
	del := rec()
	copy(del, short)
	del[0], del[11], del[26], del[28] = 0xE5, 0x20, 5, 100 // deleted, first cluster 5, 100 bytes
	raw := append(append(append([]byte{}, reused...), slot...), del...)
	l := &fatLayout{maxEntries: 100, bits: 32}
	// a whole long name with a longer extension than 8.3 allows (".heic" → "HEI") is kept
	hs := []byte("IMG_00~1HEI")
	hslot := rec()
	hslot[0], hslot[11], hslot[13] = 0xE5, 0x0F, shortSum(hs)
	for i, c := range utf16.Encode([]rune("IMG_0001.heic")) { // 13 characters: fills the slot
		hslot[lfnSlots[i]], hslot[lfnSlots[i]+1] = byte(c), byte(c>>8)
	}
	hdel := rec()
	copy(hdel, hs)
	hdel[0], hdel[11], hdel[26], hdel[28] = 0xE5, 0x20, 6, 100
	for _, e := range l.entries(append(append([]byte{}, hslot...), hdel...)) {
		if e.name != "IMG_0001.heic" {
			t.Fatalf("a whole long name was replaced: %q", e.name)
		}
	}
	for _, e := range l.entries(raw) {
		if e.deleted && !strings.HasSuffix(strings.ToLower(e.name), ".jpg") {
			t.Fatalf("deleted file named %q: its extension (and so its checks) lost", e.name)
		}
	}
}

// errAt fails reads that touch [from, to).
type errAt struct {
	r        io.ReaderAt
	from, to int64
}

func (e errAt) ReadAt(p []byte, off int64) (int, error) {
	if off < e.to && off+int64(len(p)) > e.from {
		return 0, errors.New("I/O error")
	}
	return e.r.ReadAt(p, off)
}

// Adversarial pass S10: one bad sector in the first FAT made undelete impossible, though cards
// keep a second copy of the FAT right after it.
func TestFATLayoutUsesTheSecondFAT(t *testing.T) {
	b, err := os.ReadFile(image(t, "fat32", 64<<20, tree(false)))
	if err != nil {
		t.Fatal(err)
	}
	bps, reserved := int64(le16(b[11:])), int64(le16(b[14:]))
	fatOff := reserved * bps
	if _, err := readFATLayout(errAt{bytes.NewReader(b), fatOff + 512, fatOff + 1024}, "fat32"); err != nil {
		t.Fatalf("a bad sector in FAT #1: %v", err)
	}
}
