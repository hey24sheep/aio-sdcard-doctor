package fsread

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/diskfs/go-diskfs/filesystem/fat12"
	"github.com/diskfs/go-diskfs/filesystem/fat16"
	"github.com/diskfs/go-diskfs/filesystem/fat32"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/diskfs/go-diskfs/partition/mbr"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// fill is deterministic content distinct per tag.
func fill(n int, tag string) []byte {
	return bytes.Repeat([]byte(tag+"\n"), n/(len(tag)+1)+1)[:n]
}

// tree is the recipe every fixture image is built from (same as scripts/gen-fs-images.sh).
func tree(unicode bool) map[string][]byte {
	t := map[string][]byte{
		"DCIM/Camera/IMG_0001.jpg":    fill(5000, "jpg1"),
		"a/b/c/d/deep.txt":            fill(300, "deep"),
		"Docs/empty.txt":              {},
		"Docs/cluster.bin":            fill(4096, "cluster"),
		"Docs/cluster1.bin":           fill(4097, "cluster1"),
		"big.bin":                     fill(300_000, "big"),
		"Android/.nomedia/hidden.jpg": fill(3000, "hidden"), // FAT: hidden attribute
		"Android/system.dat":          fill(200, "system"),  // FAT: system attribute
	}
	if unicode { // go-diskfs's FAT writer can't make short names for these, so only ext4 uses them
		t["हिंदी/नमस्ते.txt"] = fill(100, "hindi")
		t["Ñandú/foto.jpg"] = fill(100, "enye")
		t["emoji 😀/smile.txt"] = fill(100, "emoji")
	}
	return t
}

type writableFS interface {
	Mkdir(string) error
	OpenFile(string, int) (filesystem.File, error)
}

func populate(t *testing.T, w writableFS, files map[string][]byte, root string) {
	t.Helper()
	for name, data := range files {
		if dir := path.Dir(name); dir != "." {
			if err := w.Mkdir(root + dir); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		f, err := w.OpenFile(root+name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if a, ok := f.(interface{ SetHidden(bool) error }); ok && strings.Contains(name, "hidden") {
			a.SetHidden(true)
		}
		if a, ok := f.(interface{ SetSystem(bool) error }); ok && strings.Contains(name, "system") {
			a.SetSystem(true)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		f.Close()
	}
}

func newImage(t *testing.T, size int64) (backend.Storage, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	b, err := file.CreateFromPath(p, size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, p
}

// buildFS writes a file system of kind at [start, start+size) of b.
func buildFS(t *testing.T, b backend.Storage, kind string, start, size int64, files map[string][]byte) {
	t.Helper()
	var w writableFS
	var err error
	switch kind {
	case "fat12":
		w, err = fat12.Create(b, size, start, 512, "FAT12", true)
	case "fat16":
		w, err = fat16.Create(b, size, start, 512, "FAT16", true)
	case "fat32":
		w, err = fat32.Create(b, size, start, 512, "FAT32", true)
	case "ext4":
		w, err = ext4.Create(b, size, start, 512, &ext4.Params{VolumeName: "EXT4"})
	case "iso9660":
		ws := t.TempDir()
		var iso *iso9660.FileSystem
		iso, err = iso9660.Create(b, size, start, 2048, ws)
		if err == nil {
			populate(t, iso, files, "")
			err = iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "ISO"})
		}
		if err != nil {
			t.Fatalf("iso: %v", err)
		}
		return
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	if err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
	root := "/"
	if kind == "ext4" {
		root = "" // go-diskfs ext4 takes io/fs paths
	}
	populate(t, w, files, root)
}

// dotFix: go-diskfs's FAT writer silently drops names starting with ".", so FAT fixtures are
// built with "xnomedia1" (9 chars forces a long-name entry) and patchDotNames rewrites that
// long name to ".nomedia" in the image (its checksum covers only the short name).
func dotFix(kind string, files map[string][]byte) map[string][]byte {
	if !strings.HasPrefix(kind, "fat") {
		return files
	}
	out := map[string][]byte{}
	for k, v := range files {
		out[strings.ReplaceAll(k, "/.nomedia/", "/xnomedia1/")] = v
	}
	return out
}

// lfnSlots are the byte offsets of the 13 UTF-16 characters in a long-name entry.
var lfnSlots = []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

func patchDotNames(t *testing.T, p string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	from := []rune("xnomedia1")
	to := append([]rune(".nomedia"), 0)
	patched := 0
	for off := 0; off+32 <= len(b); off += 32 {
		e := b[off : off+32]
		if e[11] != 0x0F || e[0]&0x3F != 1 {
			continue
		}
		match := true
		for i, r := range from {
			if rune(e[lfnSlots[i]])|rune(e[lfnSlots[i]+1])<<8 != r {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		for i, o := range lfnSlots {
			c := rune(0xFFFF)
			if i < len(to) {
				c = to[i]
			}
			e[o], e[o+1] = byte(c), byte(c>>8)
		}
		patched++
	}
	if patched > 0 {
		os.WriteFile(p, b, 0o644)
	}
}

// image builds a superfloppy (no partition table) image and returns its path.
func image(t *testing.T, kind string, size int64, files map[string][]byte) string {
	b, p := newImage(t, size)
	buildFS(t, b, kind, 0, size, dotFix(kind, files))
	b.Close()
	patchDotNames(t, p)
	return p
}

// partitioned builds an MBR or GPT disk with a FAT32 partition and a FAT16 partition.
func partitioned(t *testing.T, table string, files map[string][]byte) string {
	const size, p1, p1n, p2, p2n = 96 << 20, 2048, 40 << 11, 2048 + 40<<11, 40 << 11 // sectors
	b, p := newImage(t, size)
	w, err := b.Writable()
	if err != nil {
		t.Fatal(err)
	}
	switch table {
	case "mbr":
		tb := &mbr.Table{LogicalSectorSize: 512, PhysicalSectorSize: 512, Partitions: []*mbr.Partition{
			{Type: mbr.Fat32LBA, Start: p1, Size: p1n}, {Type: mbr.Fat16, Start: p2, Size: p2n}}}
		err = tb.Write(w, size)
	case "gpt":
		tb := &gpt.Table{LogicalSectorSize: 512, PhysicalSectorSize: 512, ProtectiveMBR: true, Partitions: []*gpt.Partition{
			{Index: 1, Start: p1, End: p1 + p1n - 1, Type: gpt.MicrosoftBasicData, Name: "P1"},
			{Index: 2, Start: p2, End: p2 + p2n - 1, Type: gpt.MicrosoftBasicData, Name: "P2"}}}
		err = tb.Write(w, size)
	}
	if err != nil {
		t.Fatal(err)
	}
	buildFS(t, b, "fat32", p1*512, p1n*512, dotFix("fat32", files))
	buildFS(t, b, "fat16", p2*512, p2n*512, map[string][]byte{"second.txt": fill(10, "two")})
	b.Close()
	patchDotNames(t, p)
	return p
}

func openImage(t *testing.T, p string) (io.ReaderAt, int64) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, _ := f.Stat()
	return f, st.Size()
}

// checkTree asserts src holds exactly want (names, sizes, content), ignoring extra empty dirs.
func checkTree(t *testing.T, src source.Source, want map[string][]byte) {
	t.Helper()
	got := map[string]bool{}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		got[p] = true
		w, ok := want[p]
		if !ok {
			return fmt.Errorf("unexpected file %q", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() != int64(len(w)) {
			return fmt.Errorf("%s: size %d want %d", p, info.Size(), len(w))
		}
		r, size, c, err := src.OpenAt(p)
		if err != nil {
			return fmt.Errorf("%s: open: %w", p, err)
		}
		defer c.Close()
		buf := make([]byte, size)
		if n, err := r.ReadAt(buf, 0); n != int(size) && !(err == io.EOF && n == int(size)) {
			return fmt.Errorf("%s: read %d/%d: %v", p, n, size, err)
		}
		if sha256.Sum256(buf) != sha256.Sum256(w) {
			return fmt.Errorf("%s: content differs", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := range want {
		if !got[p] && !strings.HasSuffix(p, "/") {
			t.Errorf("missing %s", p)
		}
	}
}
