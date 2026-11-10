package fsread

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// fatGeom is the test's own reading of a FAT boot sector (independent of fatdel.go).
type fatGeom struct {
	bits                     int
	fatOff, fatLen, cs, data int64
	nfats                    int64
	rootOff, rootLen         int64
}

func geom(img []byte, kind string) fatGeom {
	le := binary.LittleEndian
	bps, spc := int64(le.Uint16(img[11:])), int64(img[13])
	g := fatGeom{cs: bps * spc, fatOff: int64(le.Uint16(img[14:])) * bps, nfats: int64(img[16])}
	fs := int64(le.Uint16(img[22:]))
	if fs == 0 {
		fs = int64(le.Uint32(img[36:]))
	}
	g.fatLen = fs * bps
	g.rootOff = g.fatOff + g.nfats*g.fatLen
	g.rootLen = (int64(le.Uint16(img[17:]))*32 + bps - 1) / bps * bps
	g.data = g.rootOff + g.rootLen
	g.bits = map[string]int{"fat12": 12, "fat16": 16, "fat32": 32}[kind]
	return g
}

func (g fatGeom) get(img []byte, c int64) int64 {
	le := binary.LittleEndian
	b := img[g.fatOff:]
	switch g.bits {
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

func (g fatGeom) set(img []byte, c, v int64) {
	le := binary.LittleEndian
	for k := int64(0); k < g.nfats; k++ {
		b := img[g.fatOff+k*g.fatLen:]
		switch g.bits {
		case 12:
			o := c * 3 / 2
			old := le.Uint16(b[o:])
			if c&1 == 1 {
				le.PutUint16(b[o:], old&0x000F|uint16(v<<4))
			} else {
				le.PutUint16(b[o:], old&0xF000|uint16(v&0xFFF))
			}
		case 16:
			le.PutUint16(b[c*2:], uint16(v))
		default:
			le.PutUint32(b[c*4:], uint32(v))
		}
	}
}

func (g fatGeom) eoc() int64 { return map[int]int64{12: 0xFFF, 16: 0xFFFF, 32: 0x0FFFFFFF}[g.bits] }

// entry finds the 32-byte directory record of an 8.3 name ("GONE    JPG").
func entry(t *testing.T, img []byte, short string) []byte {
	t.Helper()
	for i := 0; ; {
		j := bytes.Index(img[i:], []byte(short))
		if j < 0 {
			t.Fatalf("no record %q", short)
		}
		if (i+j)%32 == 0 {
			return img[i+j : i+j+32]
		}
		i += j + 1
	}
}

func firstClus(e []byte, g fatGeom) int64 {
	c := int64(binary.LittleEndian.Uint16(e[26:]))
	if g.bits == 32 {
		c |= int64(binary.LittleEndian.Uint16(e[20:])) << 16
	}
	return c
}

// osDelete deletes a file the way an OS does: 0xE5 on its record (and its long-name records)
// and its cluster chain freed. Returns its first cluster.
func osDelete(t *testing.T, img []byte, g fatGeom, short string) int64 {
	t.Helper()
	e := entry(t, img, short)
	for k := 32; ; k += 32 { // long-name records just before it
		start := len(img) - cap(e) - k // offset of the record k bytes before e
		if start < 0 || img[start+11] != 0x0F {
			break
		}
		img[start] = 0xE5
	}
	e[0] = 0xE5
	c := firstClus(e, g)
	for n := 0; c >= 2 && c < g.eoc()-7 && n < 1<<20; n++ {
		next := g.get(img, c)
		g.set(img, c, 0)
		c = next
	}
	return firstClus(e, g)
}

func fatImage(t *testing.T, kind string, files map[string][]byte) []byte {
	t.Helper()
	size := map[string]int64{"fat12": 4 << 20, "fat16": 32 << 20, "fat32": 64 << 20}[kind]
	b, p := newImage(t, size)
	buildFS(t, b, kind, 0, size, files)
	img, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func deletedOf(t *testing.T, img []byte, kind string) map[string]source.Deleted {
	t.Helper()
	src, err := Open(bytes.NewReader(img), Volume{FS: kind, Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]source.Deleted{}
	if err := src.(source.Undeleter).Deleted(context.Background(), func(d source.Deleted) error {
		out[d.Path] = d
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeletedStates(t *testing.T) {
	for _, kind := range []string{"fat12", "fat16", "fat32"} {
		t.Run(kind, func(t *testing.T) {
			gone, frag := fill(9000, "gone"), fill(100_000, "frag")
			if kind == "fat12" {
				frag = fill(20_000, "frag")
			}
			img := fatImage(t, kind, map[string][]byte{
				"DCIM/KEEP.JPG": fill(5000, "keep"), "DCIM/GONE.JPG": gone, "OVER.JPG": fill(100, "over"),
				"FRAG.BIN": frag, "OLD/PIC.JPG": fill(3000, "pic"),
			})
			g := geom(img, kind)
			osDelete(t, img, g, "GONE    JPG")
			osDelete(t, img, g, "PIC     JPG")
			osDelete(t, img, g, "OLD        ")
			osDelete(t, img, g, "OVER    JPG")
			// the freed cluster of OVER.JPG went to KEEP.JPG: point OVER's record at KEEP's cluster
			keep := firstClus(entry(t, img, "KEEP    JPG"), g)
			ov := entry(t, img, "\xE5VER    JPG")
			binary.LittleEndian.PutUint16(ov[26:], uint16(keep))
			binary.LittleEndian.PutUint16(ov[20:], uint16(keep>>16)*uint16(g.bits/32))
			fc := osDelete(t, img, g, "FRAG    BIN")
			g.set(img, fc+1, g.eoc()) // something new took the middle of FRAG's old run

			got := deletedOf(t, img, kind)
			var names []string
			for p := range got {
				names = append(names, p)
			}
			find := func(suffix string) source.Deleted {
				for p, d := range got {
					if strings.HasSuffix(p, suffix) {
						return d
					}
				}
				t.Fatalf("no deleted entry ending %q in %v", suffix, names)
				return source.Deleted{}
			}
			d := find("ONE.JPG")
			if d.State != "recoverable" || d.Size != int64(len(gone)) || len(d.Extents) != 1 || !strings.HasPrefix(d.Path, "DCIM/") {
				t.Fatalf("GONE: %+v", d)
			}
			if off := d.Extents[0][0]; !bytes.Equal(img[off:off+d.Size], gone) {
				t.Fatal("GONE extent does not hold its bytes")
			}
			if d.MTime.IsZero() {
				t.Error("no modified time")
			}
			if d := find("VER.JPG"); d.State != "overwritten" {
				t.Errorf("OVER: %+v", d)
			}
			if d := find("RAG.BIN"); d.State != "fragmented-unknown" {
				t.Errorf("FRAG: %+v", d)
			}
			if d := find("IC.JPG"); d.State != "recoverable" || !strings.Contains(d.Path, "LD/") {
				t.Errorf("file in a deleted folder: %+v", d)
			}
			for _, live := range []string{"DCIM/KEEP.JPG"} {
				if _, ok := got[live]; ok {
					t.Errorf("live file %s reported deleted", live)
				}
			}
		})
	}
}

func TestDeletedEmptyIsMetadataDamaged(t *testing.T) {
	img := fatImage(t, "fat16", map[string][]byte{"ZERO.TXT": {}, "A.TXT": fill(10, "a")})
	osDelete(t, img, geom(img, "fat16"), "ZERO    TXT")
	if d := deletedOf(t, img, "fat16")["_ERO.TXT"]; d.State != "metadata-damaged" || d.Extents != nil {
		t.Fatalf("%+v", d)
	}
}

func TestLiveExtentsFAT(t *testing.T) {
	keep := fill(70_000, "keep")
	img := fatImage(t, "fat32", map[string][]byte{"DCIM/KEEP.JPG": keep, "GONE.JPG": fill(500, "gone")})
	osDelete(t, img, geom(img, "fat32"), "GONE    JPG")
	src, _ := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	var got []byte
	paths := map[string]bool{}
	err := src.(source.LiveMapper).LiveExtents(context.Background(), func(p string, off, n int64) error {
		paths[p] = true
		if p == "DCIM/KEEP.JPG" {
			got = append(got, img[off:off+n]...)
		}
		return nil
	})
	if err != nil || !bytes.Equal(got, keep) || paths["GONE.JPG"] || len(paths) != 1 {
		t.Fatalf("err %v, %d bytes, paths %v", err, len(got), paths)
	}
}

func TestUndeleteUnsupportedFS(t *testing.T) {
	v := &vol{drv: &diskfsDriver{}}
	if err := v.Deleted(context.Background(), func(source.Deleted) error { return nil }); err == nil {
		t.Fatal("a driver without deleted records answered")
	}
}

// TestFAT16DirectoryPointingAtRoot (review I6): a damaged directory record whose first cluster is 0
// (on FAT12/16 that reads the root again) can't make the listing repeat itself.
func TestFAT16DirectoryPointingAtRoot(t *testing.T) {
	img := fatImage(t, "fat16", map[string][]byte{"SUB/X.TXT": fill(10, "x"), "SUB2/Y.TXT": fill(10, "y"), "GONE.JPG": fill(3000, "g")})
	g := geom(img, "fat16")
	osDelete(t, img, g, "GONE    JPG")
	for _, d := range []string{"SUB        ", "SUB2       "} {
		e := entry(t, img, d)
		e[26], e[27] = 0, 0
	}
	got := deletedOf(t, img, "fat16")
	if len(got) != 1 {
		t.Fatalf("%d deleted entries listed: %v", len(got), got)
	}
}
