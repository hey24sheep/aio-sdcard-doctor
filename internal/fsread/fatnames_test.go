package fsread

import (
	"bytes"
	"io/fs"
	"os"
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
