package fsread

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// Adversarial pass 2: damaged FAT metadata must give a partial answer and an error, never an
// unreadable volume, an endless walk or a worker that eats memory until it is killed.

// within fails the test if fn doesn't finish in d (a looping parser would run forever).
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v", d)
	}
}

// OR1: one bad or zeroed sector in FAT #1 made the whole volume unreadable, though FAT #2 holds
// the same table.
func TestBadFirstFATSectorStillOpens(t *testing.T) {
	for _, kind := range []string{"fat32", "fat16"} {
		files := map[string][]byte{"big.bin": fill(2<<20, "big"), "DCIM/x.jpg": fill(5000, "x")}
		img := fatImage(t, kind, files)
		g := geom(img, kind)
		if g.get(img, 200) == 0 {
			t.Fatalf("%s: the damaged FAT sector must be in use for this test", kind)
		}
		t.Run(kind+"/unreadable", func(t *testing.T) {
			r := errAt{bytes.NewReader(img), g.fatOff + 512, g.fatOff + 1024}
			src, err := Open(r, Volume{FS: kind, Size: int64(len(img))})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			checkTree(t, src, files)
		})
		t.Run(kind+"/zeroed", func(t *testing.T) { // an image of the card has zeros there
			z := append([]byte(nil), img...)
			clear(z[g.fatOff+512 : g.fatOff+1024])
			src, err := Open(bytes.NewReader(z), Volume{FS: kind, Size: int64(len(z))})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			checkTree(t, src, files)
		})
	}
}

// OR4: a file whose cluster chain loops back on itself made go-diskfs allocate without bound.
// The clusters the chain does reach are readable; the rest is an error, not a short file.
func TestLoopedFileChainReadsPrefixThenError(t *testing.T) {
	content := fill(20000, "aaa")
	img := fatImage(t, "fat32", map[string][]byte{"a.txt": content, "z.txt": fill(10, "z")})
	g := geom(img, "fat32")
	c := firstClus(entry(t, img, "A       TXT"), g)
	g.set(img, c, c) // the first cluster points at itself
	src, err := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() {
		f, err := src.Open("a.txt")
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err == nil {
			t.Errorf("read %d of %d bytes with no error", len(b), len(content))
		}
		if len(b) < int(g.cs) || !bytes.Equal(b, content[:len(b)]) {
			t.Errorf("got %d bytes, want the first cluster (%d) intact", len(b), g.cs)
		}
	})
}

// OR4: a folder whose chain loops is listed once.
func TestLoopedFolderChainIsListedOnce(t *testing.T) {
	img := fatImage(t, "fat32", map[string][]byte{"DCIM/a.txt": fill(200, "aaa")})
	g := geom(img, "fat32")
	c := firstClus(entry(t, img, "DCIM       "), g)
	g.set(img, c, c)
	src, err := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() {
		es, err := fs.ReadDir(src, "DCIM")
		if err != nil || len(es) != 1 || es[0].Name() != "a.txt" {
			t.Errorf("DCIM: %v, %v", es, err)
		}
	})
}

// OR3: a folder record pointing back at a parent folder made DCIM/SUB/SUB/… endless. The loop
// is reported on the folder that closes it; everything else is listed.
func TestFolderPointingAtItsParentIsReported(t *testing.T) {
	img := fatImage(t, "fat32", map[string][]byte{"DCIM/SUB/a.txt": fill(200, "aaa"), "DCIM/b.txt": fill(100, "b")})
	g := geom(img, "fat32")
	dc := firstClus(entry(t, img, "DCIM       "), g)
	e := entry(t, img, "SUB        ")
	e[26], e[27], e[20], e[21] = byte(dc), byte(dc>>8), byte(dc>>16), byte(dc>>24)
	src, err := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, func() {
		var seen, failed []string
		_ = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				failed = append(failed, p)
				return nil
			}
			seen = append(seen, p)
			if len(seen) > 100 {
				return fs.SkipAll
			}
			return nil
		})
		if len(seen) > 10 || strings.Join(failed, ",") != "DCIM/SUB" {
			t.Errorf("walk saw %d entries %v, failed %v; want DCIM/SUB reported", len(seen), seen[:min(len(seen), 6)], failed)
		}
		if _, err := fs.ReadDir(src, "DCIM/SUB"); !errors.Is(err, errDirLoop) {
			t.Errorf("DCIM/SUB: %v, want errDirLoop", err)
		}
	})
}

// endless is a file system whose every folder holds a folder: what an ext4 or NTFS folder
// record pointing back at a parent looks like from above.
type endless struct{}

func (endless) list(string) ([]node, error) { return []node{{name: "d", dir: true}}, nil }
func (endless) open(string, node) (io.ReaderAt, io.Closer, error) {
	return nil, nil, fs.ErrNotExist
}

// OR3 on any file system: the walk stops at maxDepth with an error on the folder that is too deep.
func TestEndlessFolderTreeStops(t *testing.T) {
	v := newVol(endless{}, "ext4", Volume{})
	n, failed := 0, 0
	within(t, 3*time.Second, func() {
		_ = fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, errTooDeep) {
					t.Errorf("%s: %v", p, err)
				}
				failed++
				return nil
			}
			n++
			return nil
		})
	})
	if n != maxDepth+2 || failed != 1 { // "." and d, d/d, … down to maxDepth slashes
		t.Errorf("walked %d folders with %d errors, want %d and 1", n, failed, maxDepth+2)
	}
}

// A long name whose checksum doesn't match its short name: go-diskfs lists the long name, our
// parser the short one. The folder must still be listed and read, as before.
func TestFolderWithMismatchedLongNameIsRead(t *testing.T) {
	files := map[string][]byte{"DCIM/Long Folder Name/a.txt": fill(3000, "a")}
	img := fatImage(t, "fat32", files)
	e := entry(t, img, "LONGFO~1   ")
	off := cap(img) - cap(e) // the short entry's offset; its long-name entries are just before it
	for k := off - 32; k >= 0 && img[k+11] == 0x0F; k -= 32 {
		img[k+13] ^= 0xFF
	}
	src, err := Open(bytes.NewReader(img), Volume{FS: "fat32", Size: int64(len(img))})
	if err != nil {
		t.Fatal(err)
	}
	checkTree(t, src, files)
}
