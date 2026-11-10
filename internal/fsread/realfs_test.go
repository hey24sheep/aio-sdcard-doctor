package fsread

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// TestRealFSImages reads the images scripts/gen-fs-images.sh builds with the real mkfs tools
// (testdata/realfs; AIO_FSIMAGES points at a freshly built set instead, as the CI job
// "fsimages" does). This is where exFAT, and FAT/NTFS written by Linux, get their golden check.
func TestRealFSImages(t *testing.T) {
	dir := os.Getenv("AIO_FSIMAGES")
	if dir == "" {
		dir = filepath.Join("testdata", "realfs")
	}
	recipe := func(cs int) map[string][]byte {
		return map[string][]byte{
			"DCIM/Camera/IMG_0001.jpg": fill(5000, "jpg1"),
			"a/b/c/d/deep.txt":         fill(300, "deep"),
			"Docs/empty.txt":           {},
			"Docs/cluster.bin":         fill(cs, "cluster"),
			"Docs/cluster1.bin":        fill(cs+1, "cluster1"),
			"हिंदी/नमस्ते.txt":         fill(100, "hindi"),
			"emoji 😀/smile.txt":        fill(100, "emoji"),
		}
	}
	images, _ := filepath.Glob(filepath.Join(dir, "*.img.gz"))
	if len(images) == 0 {
		t.Fatalf("no images in %s", dir)
	}
	for _, p := range images {
		name := strings.TrimSuffix(filepath.Base(p), ".img.gz")
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			z, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(z)
			r := bytes.NewReader(b)
			vs, err := Probe(r, r.Size())
			if err != nil || len(vs) == 0 {
				t.Fatalf("probe: %v %v", vs, err)
			}
			src, err := Open(r, vs[0])
			if err != nil {
				t.Fatal(err)
			}
			cs := 4096
			if strings.HasPrefix(name, "fat") || name == "iso9660" || strings.HasPrefix(name, "disk-") {
				cs = 2048
			}
			for rel, want := range recipe(cs) {
				if strings.HasPrefix(rel, "emoji") && strings.HasPrefix(name, "fat") {
					continue // mtools (which builds fat*.img) stores UCS-2 only: 😀 becomes U+F600
				}
				ra, size, c, err := src.OpenAt(rel)
				if err != nil {
					t.Errorf("%s: %v", rel, err)
					continue
				}
				got := make([]byte, size)
				ra.ReadAt(got, 0)
				c.Close()
				if !bytes.Equal(got, want) {
					t.Errorf("%s: content differs (%d bytes, want %d)", rel, size, len(want))
				}
			}
			// deleted files the script left behind (FAT and exFAT; NTFS undelete isn't supported yet)
			if !Capabilities(&vs[0], nil)[1].Enabled || strings.HasPrefix(name, "disk-") {
				return // disk-* test partition tables; the script deletes nothing on them
			}
			states := map[string]source.Deleted{}
			if err := src.(source.Undeleter).Deleted(context.Background(), func(d source.Deleted) error {
				states[path.Base(d.Path)[1:]] = d // FAT loses a deleted short name's first letter
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Whether a deleted file's space was reused depends on the OS's allocator, so the truth
			// is what live files occupy now: untouched → recoverable, reused → not.
			var live [][2]int64
			if lm, ok := src.(source.LiveMapper); ok {
				lm.LiveExtents(context.Background(), func(_ string, off, n int64) error {
					live = append(live, [2]int64{off, n})
					return nil
				})
			}
			reused := func(d source.Deleted) bool {
				for _, x := range d.Extents {
					for _, l := range live {
						if x[0] < l[0]+l[1] && l[0] < x[0]+x[1] {
							return true
						}
					}
				}
				return false
			}
			for _, k := range []struct{ key, file, tag string }{{"ELETED.JPG", "DELETED.JPG", "deleted"}, {"VERWR.JPG", "OVERWR.JPG", "overwritten"}} {
				d, ok := states[k.key]
				switch {
				case !ok || len(d.Extents) == 0:
					t.Errorf("%s not listed: %+v", k.file, d)
				case live != nil && reused(d) && d.State == "recoverable":
					t.Errorf("%s: space reused by a live file, but %s", k.file, d.State)
				case live != nil && !reused(d) && d.State != "recoverable":
					t.Errorf("%s: space untouched, but %s", k.file, d.State)
				case d.State == "recoverable" && k.file == "DELETED.JPG" &&
					!bytes.Equal(b[vs[0].Offset+d.Extents[0][0]:][:d.Size], fill(cs*2, k.tag)):
					t.Errorf("%s: recovered content differs", k.file)
				default:
					t.Logf("%s: %s", k.file, d.State)
				}
			}
		})
	}
}
