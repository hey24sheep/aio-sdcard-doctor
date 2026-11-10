package testutil

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// DeleteFATFile deletes a file in a FAT image file the way an OS does: 0xE5 on its directory
// record (and its long-name records) and its cluster chain freed in every FAT. short is the
// 11-byte 8.3 name ("GONE    JPG"). Returns the file's first cluster.
func DeleteFATFile(t testing.TB, imgPath, short string) int64 {
	t.Helper()
	img, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	c := DeleteFAT(t, img, short)
	if err := os.WriteFile(imgPath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return c
}

// DeleteFAT is DeleteFATFile on an image in memory.
func DeleteFAT(t testing.TB, img []byte, short string) int64 {
	t.Helper()
	le := binary.LittleEndian
	bps, spc := int64(le.Uint16(img[11:])), int64(img[13])
	fatOff, nfats := int64(le.Uint16(img[14:]))*bps, int64(img[16])
	fatLen := int64(le.Uint16(img[22:]))
	if fatLen == 0 {
		fatLen = int64(le.Uint32(img[36:]))
	}
	fatLen *= bps
	total := int64(le.Uint16(img[19:]))
	if total == 0 {
		total = int64(le.Uint32(img[32:]))
	}
	rootLen := (int64(le.Uint16(img[17:]))*32 + bps - 1) / bps * bps
	data := fatOff + nfats*fatLen + rootLen
	clusters := (total*bps - data) / (bps * spc)
	bits := 32
	switch {
	case clusters < 4085:
		bits = 12
	case clusters < 65525:
		bits = 16
	}
	get := func(c int64) int64 {
		b := img[fatOff:]
		switch bits {
		case 12:
			v := int64(le.Uint16(b[c*3/2:]))
			if c&1 == 1 {
				return v >> 4
			}
			return v & 0xFFF
		case 16:
			return int64(le.Uint16(b[c*2:]))
		}
		return int64(le.Uint32(b[c*4:]) & 0x0FFFFFFF)
	}
	free := func(c int64) {
		for k := int64(0); k < nfats; k++ {
			b := img[fatOff+k*fatLen:]
			switch bits {
			case 12:
				o := c * 3 / 2
				if c&1 == 1 {
					le.PutUint16(b[o:], le.Uint16(b[o:])&0x000F)
				} else {
					le.PutUint16(b[o:], le.Uint16(b[o:])&0xF000)
				}
			case 16:
				le.PutUint16(b[c*2:], 0)
			default:
				le.PutUint32(b[c*4:], 0)
			}
		}
	}
	at := -1
	for i := 0; ; {
		j := bytes.Index(img[i:], []byte(short))
		if j < 0 {
			t.Fatalf("no directory record %q", short)
		}
		if (i+j)%32 == 0 && img[i+j+11] != 0x0F {
			at = i + j
			break
		}
		i += j + 1
	}
	for k := at - 32; k >= 0 && img[k+11] == 0x0F; k -= 32 {
		img[k] = 0xE5
	}
	img[at] = 0xE5
	first := int64(le.Uint16(img[at+26:]))
	if bits == 32 {
		first |= int64(le.Uint16(img[at+20:])) << 16
	}
	eoc := map[int]int64{12: 0xFF8, 16: 0xFFF8, 32: 0x0FFFFFF8}[bits]
	for c, n := first, 0; c >= 2 && c < eoc && n < 1<<22; n++ {
		next := get(c)
		free(c)
		c = next
	}
	return first
}
