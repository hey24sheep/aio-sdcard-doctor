package fsread

import (
	"bytes"
	"io"
	"testing"
)

// probeAndOpen probes r and checks it holds one volume of the file system want.
func probeAndOpen(t *testing.T, r io.ReaderAt, size int64, want string) Volume {
	t.Helper()
	vs, err := Probe(r, size)
	if err != nil || len(vs) != 1 || vs[0].FS != want {
		t.Fatalf("probe: %+v %v, want one %s volume", vs, err, want)
	}
	return vs[0]
}

// Adversarial pass 3: a bad boot sector left only a deep scan, though FAT32 keeps a copy of it
// at sector 6 and exFAT a copy of its boot region at sector 12.
func TestBadBootSectorUsesTheBackup(t *testing.T) {
	t.Run("fat32", func(t *testing.T) {
		files := map[string][]byte{"DCIM/a.jpg": fill(5000, "a"), "b.txt": fill(100, "b")}
		img := fatImage(t, "fat32", files)
		if fatKind(img[6*512:7*512]) != "fat32" {
			t.Fatal("the test image has no backup boot sector")
		}
		for name, r := range map[string]io.ReaderAt{
			"zeroed":     bytes.NewReader(append(make([]byte, 512), img[512:]...)),
			"unreadable": badAreas{bytes.NewReader(img), [][2]int64{{0, 512}}},
		} {
			t.Run(name, func(t *testing.T) {
				v := probeAndOpen(t, r, int64(len(img)), "fat32")
				src, err := Open(r, v)
				if err != nil {
					t.Fatal(err)
				}
				checkTree(t, src, files)
			})
		}
	})
	t.Run("exfat", func(t *testing.T) {
		img := realImage(t, "exfat")
		for name, r := range map[string]io.ReaderAt{
			"zeroed":     bytes.NewReader(append(make([]byte, 512), img[512:]...)),
			"unreadable": badAreas{bytes.NewReader(img), [][2]int64{{0, 512}}},
		} {
			t.Run(name, func(t *testing.T) {
				v := probeAndOpen(t, r, int64(len(img)), "exfat")
				src, err := Open(r, v)
				if err != nil {
					t.Fatal(err)
				}
				f, err := src.Open("DCIM/Camera/IMG_0001.jpg")
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(f)
				f.Close()
				if !bytes.Equal(b, fill(5000, "jpg1")) {
					t.Errorf("read %d bytes, want the file", len(b))
				}
			})
		}
	})
}
