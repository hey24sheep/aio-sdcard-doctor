//go:build realfs

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
// (CI job "fsimages"; AIO_FSIMAGES points at its output). This is where exFAT, and FAT/NTFS
// written by Linux, get their golden check.
func TestRealFSImages(t *testing.T) {
	dir := os.Getenv("AIO_FSIMAGES")
	if dir == "" {
		t.Skip("set AIO_FSIMAGES to the output of scripts/gen-fs-images.sh")
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
			if !Capabilities(&vs[0], nil)[1].Enabled {
				return
			}
			states := map[string]source.Deleted{}
			if err := src.(source.Undeleter).Deleted(context.Background(), func(d source.Deleted) error {
				states[path.Base(d.Path)[1:]] = d // FAT loses a deleted short name's first letter
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if d := states["ELETED.JPG"]; d.State != "recoverable" || len(d.Extents) == 0 ||
				!bytes.Equal(b[vs[0].Offset+d.Extents[0][0]:][:d.Size], fill(cs*2, "deleted")) {
				t.Errorf("DELETED.JPG: %+v", d)
			}
			if d := states["VERWR.JPG"]; d.State != "overwritten" {
				t.Errorf("OVERWR.JPG: %+v", d)
			}
		})
	}
}
