package fsread

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial pass 3: a folder that can only be read in part must say so. A listing that quietly
// leaves files out makes the copy look complete while files stay behind on the card.

// badAreas fails reads that touch any of its [from, to) areas, returning what lies before it.
type badAreas struct {
	r   io.ReaderAt
	bad [][2]int64
}

func (b badAreas) ReadAt(p []byte, off int64) (int, error) {
	for _, a := range b.bad {
		if off < a[1] && off+int64(len(p)) > a[0] {
			n := 0
			if off < a[0] {
				n, _ = b.r.ReadAt(p[:a[0]-off], off)
			}
			return n, errors.New("I/O error")
		}
	}
	return b.r.ReadAt(p, off)
}

// dirChainOf follows a folder's cluster chain in the test's own reading of the FAT.
func dirChainOf(img []byte, g fatGeom, c int64) []int64 {
	var out []int64
	for i := 0; c >= 2 && c < g.eoc()-7 && i < 1000; i++ {
		out = append(out, c)
		c = g.get(img, c)
	}
	return out
}

func manyFiles(dir string, n int) map[string][]byte {
	files := map[string][]byte{}
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("%s/F%04d.JPG", dir, i)] = fill(100, fmt.Sprint(i))
	}
	return files
}

// checkPartial asserts dir lists its readable entries (want of them, every one openable) with an
// error naming dir.
func checkPartial(t *testing.T, src fs.FS, dir string, want int) {
	t.Helper()
	es, err := fs.ReadDir(src, dir)
	if err == nil || !errors.Is(err, ErrPartialFolder) || !strings.Contains(err.Error(), dir) {
		t.Errorf("ReadDir(%s) error = %v, want a partial-folder error naming it", dir, err)
	}
	if len(es) != want {
		t.Errorf("ReadDir(%s) gave %d entries, want the %d readable ones", dir, len(es), want)
	}
	for _, e := range es[:min(len(es), 3)] {
		f, err := src.Open(dir + "/" + e.Name())
		if err != nil {
			t.Errorf("open %s: %v", e.Name(), err)
			continue
		}
		f.Close()
	}
	walked := 0
	fs.WalkDir(src, dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			walked++
		}
		return nil
	})
	if walked != want {
		t.Errorf("WalkDir saw %d files under %s, want %d", walked, dir, want)
	}
}

// CI (adv3): the same damage in the folder's FIRST sector — the one holding "." and ".." —
// still hid 14 files without a word in an image (the zeroed sector reads as the folder's end).
func TestFolderFirstSectorLostIsReported(t *testing.T) {
	img := fatImage(t, "fat32", manyFiles("DCIM", 300))
	g := geom(img, "fat32")
	off := g.data + (firstClus(entry(t, img, "DCIM       "), g)-2)*g.cs
	if img[off] != '.' {
		t.Fatalf("DCIM's first sector doesn't start with its \".\" record")
	}
	lost := 0
	for k := off; k < off+512; k += 32 {
		if img[k] != 0 && img[k] != '.' && img[k+11] != 0x0F {
			lost++
		}
	}
	want := 300 - lost
	t.Run("unreadable", func(t *testing.T) {
		src, err := Open(badAreas{bytes.NewReader(img), [][2]int64{{off, off + 512}}}, Volume{FS: "fat32", Size: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		checkPartial(t, src, "DCIM", want)
	})
	t.Run("zeroed in an image", func(t *testing.T) {
		z := append([]byte(nil), img...)
		clear(z[off : off+512])
		src, err := Open(bytes.NewReader(z), Volume{FS: "fat32", Size: int64(len(z))})
		if err != nil {
			t.Fatal(err)
		}
		checkPartial(t, src, "DCIM", want)
	})
}

// OR1: one unreadable sector in a folder's second cluster hid that folder's files with no error.
func TestUnreadableFolderSectorIsReported(t *testing.T) {
	img := fatImage(t, "fat32", manyFiles("DCIM", 300))
	g := geom(img, "fat32")
	ch := dirChainOf(img, g, firstClus(entry(t, img, "DCIM       "), g))
	off := g.data + (ch[1]-2)*g.cs // a sector in DCIM's second cluster
	lost := 0                      // the files whose records sit in that sector
	for k := off; k < off+512; k += 32 {
		if img[k] != 0 && img[k+11] != 0x0F {
			lost++
		}
	}
	want := 300 - lost
	if g.cs > 512 {
		t.Fatalf("this test expects 512-byte clusters, got %d", g.cs)
	}
	t.Run("unreadable", func(t *testing.T) {
		src, err := Open(badAreas{bytes.NewReader(img), [][2]int64{{off, off + 512}}}, Volume{FS: "fat32", Size: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		checkPartial(t, src, "DCIM", want)
	})
	t.Run("unreadable, FAT #1 damaged", func(t *testing.T) {
		bad := [][2]int64{{off, off + 512}, {g.fatOff + 512, g.fatOff + 1024}}
		src, err := Open(badAreas{bytes.NewReader(img), bad}, Volume{FS: "fat32", Size: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		checkPartial(t, src, "DCIM", want)
	})
	t.Run("zeroed in an image", func(t *testing.T) {
		z := append([]byte(nil), img...)
		clear(z[off : off+512])
		src, err := Open(bytes.NewReader(z), Volume{FS: "fat32", Size: int64(len(z))})
		if err != nil {
			t.Fatal(err)
		}
		checkPartial(t, src, "DCIM", want)
	})
}

// OR1 on FAT16: an unreadable sector of the fixed root folder.
func TestUnreadableRootSectorIsReported(t *testing.T) {
	files := manyFiles("DCIM", 2)
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("R%04d.TXT", i)] = fill(10, fmt.Sprint(i))
	}
	img := fatImage(t, "fat16", files)
	g := geom(img, "fat16")
	off := g.rootOff + 512 // root sector 2: 16 short records
	src, err := Open(badAreas{bytes.NewReader(img), [][2]int64{{off, off + 512}}}, Volume{FS: "fat16", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	es, err := fs.ReadDir(src, ".")
	if !errors.Is(err, ErrPartialFolder) {
		t.Errorf("root: %v, want a partial-folder error", err)
	}
	if len(es) < 41-16 || len(es) > 41-1 {
		t.Errorf("root lists %d entries, want the readable ones", len(es))
	}
}

// A folder that reads cleanly is never called partial (the end-of-folder mark in its last
// cluster is followed by nothing).
func TestWholeFolderIsNotPartial(t *testing.T) {
	img := fatImage(t, "fat32", manyFiles("DCIM", 300))
	src, err := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	if es, err := fs.ReadDir(src, "DCIM"); err != nil || len(es) != 300 {
		t.Errorf("DCIM: %d entries, %v", len(es), err)
	}
}

// utf16le spells s as NTFS stores names.
func utf16le(s string) []byte {
	var b []byte
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// OR1 on NTFS: go-ntfs skips a file whose MFT record can't be read, and an index block it can't
// decode, without a word. The listing says so.
func TestNTFSUnreadableRecordIsReported(t *testing.T) {
	r := gunzipFixture(t, "ntfs.img.gz")
	img := make([]byte, r.Size())
	r.ReadAt(img, 0)
	name := utf16le("Hello world text document.txt")
	rec := int64(-1)
	for off := 0; off+1024 <= len(img); off += 1024 {
		m := img[off : off+1024] // the file's own record: a folder's record holds the name in its index ($I30)
		if bytes.HasPrefix(m, []byte("FILE")) && bytes.Contains(m, name) && !bytes.Contains(m, utf16le("$I30")) {
			rec = int64(off)
			break
		}
	}
	if rec < 0 {
		t.Fatal("no MFT record for the test file")
	}
	vs, err := Probe(r, r.Size())
	if err != nil || len(vs) != 1 {
		t.Fatalf("%+v %v", vs, err)
	}
	src, err := Open(badAreas{bytes.NewReader(img), [][2]int64{{rec, rec + 1024}}}, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	es, err := fs.ReadDir(src, "Folder A/Folder B")
	if !errors.Is(err, ErrPartialFolder) {
		t.Errorf("Folder B: %d entries, error %v; want a partial-folder error", len(es), err)
	}
	if es, err := fs.ReadDir(src, "Folder A"); err != nil || len(es) == 0 {
		t.Errorf("Folder A (whole): %d entries, %v", len(es), err)
	}
}

// OR2: a FAT12/16 folder record whose first cluster is 0 made go-diskfs read the root again at
// every level (the root listed hundreds of times). The folder is reported as unreadable instead.
func TestFolderRecordWithNoClusterIsReported(t *testing.T) {
	for _, kind := range []string{"fat12", "fat16"} {
		img := fatImage(t, kind, map[string][]byte{"SUB/X.TXT": fill(10, "x"), "A.TXT": fill(10, "a"), "B.TXT": fill(10, "b")})
		e := entry(t, img, "SUB        ")
		e[26], e[27] = 0, 0
		src, err := Open(bytes.NewReader(img), Volume{FS: kind, Size: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		files := 0
		var failed []string
		within(t, 10*time.Second, func() {
			fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
				switch {
				case err != nil:
					failed = append(failed, p)
				case !d.IsDir():
					files++
				}
				return nil
			})
		})
		if files != 2 || strings.Join(failed, ",") != "SUB" {
			t.Errorf("%s: walked %d files, failed %d folders %.60v; want A.TXT and B.TXT once and SUB reported", kind, files, len(failed), failed)
		}
	}
}

// OR5: a damaged boot sector claiming a FAT far bigger than the card made Open allocate gigabytes
// (the worker was killed for memory). Such a volume is refused at once, with a clear error.
func TestBootSectorBiggerThanTheCardIsRefused(t *testing.T) {
	img := fatImage(t, "fat32", map[string][]byte{"a.txt": fill(5000, "aaa")})[:4<<20]
	binary.LittleEndian.PutUint32(img[32:], 0xFFFFFFFF) // total sectors
	binary.LittleEndian.PutUint32(img[36:], 1<<21)      // sectors per FAT: 1 GiB each
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var err error
	within(t, 5*time.Second, func() {
		_, err = Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	})
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "bigger than") {
		t.Errorf("Open: %v, want the volume refused", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("Open allocated %d MiB for a 4 MiB volume", grew>>20)
	}
}

// SR5: two live records with the same name in one folder (damage chkdsk reports as duplicate
// names) were both listed, but opening either gave the first one's data. The second is listed
// under a name of its own and opens its own data.
func TestSameNameTwiceOpensEachRecord(t *testing.T) {
	for _, kind := range []string{"fat32", "fat16"} {
		a, b := fill(3000, "AAAA"), fill(7000, "BBBB")
		img := fatImage(t, kind, map[string][]byte{"A.TXT": a, "B.TXT": b, "A (2).TXT": fill(10, "taken")})
		copy(entry(t, img, "B       TXT")[:11], "A       TXT")
		src, err := Open(bytes.NewReader(img), Volume{FS: kind, Size: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]byte{}
		es, err := fs.ReadDir(src, ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			b, err := fs.ReadFile(src, e.Name())
			if err != nil {
				t.Errorf("%s: %s: %v", kind, e.Name(), err)
			}
			got[e.Name()] = b
		}
		first, second := got["A.TXT"], got["A (3).TXT"] // in the folder's order, which the image builder picks
		if bytes.Equal(first, b) {
			first, second = second, first
		}
		if len(got) != 3 || !bytes.Equal(first, a) || !bytes.Equal(second, b) {
			t.Errorf("%s: listed %d names; A.TXT %d bytes, A (3).TXT %d bytes; want 3000 and 7000", kind, len(got), len(got["A.TXT"]), len(got["A (3).TXT"]))
		}
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(int64(len(p)))
	return c.r.ReadAt(p, off)
}

// OR6/SR6: files copied in size order alternate between folders, and each open re-read its
// folder from the card (50 MB of reads for 400 small files). Folders read once are kept.
func TestOpeningAcrossFoldersReadsEachFolderOnce(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("DCIM/100/IMG_%04d.JPG", i)] = fill(600, "a")
		files[fmt.Sprintf("DCIM/101/IMG_%04d.JPG", i)] = fill(600, "b")
	}
	img := fatImage(t, "fat32", files)
	c := &countingReader{r: bytes.NewReader(img)}
	src, err := Open(c, Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	c.n.Store(0)
	for i := 0; i < 200; i++ {
		for _, d := range []string{"100", "101"} {
			if _, err := fs.ReadFile(src, fmt.Sprintf("DCIM/%s/IMG_%04d.JPG", d, i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := c.n.Load(); n > 1<<20 { // the data is 234 KiB, the four folders about 30 KiB
		t.Errorf("400 files of 600 bytes read %d KiB from the card", n>>10)
	}
}
