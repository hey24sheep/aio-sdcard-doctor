package fsread

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func probeOne(t *testing.T, p string) (Volume, io.ReaderAt) {
	t.Helper()
	r, size := openImage(t, p)
	vs, err := Probe(r, size)
	if err != nil || len(vs) != 1 {
		t.Fatalf("Probe = %+v, %v", vs, err)
	}
	return vs[0], r
}

func TestReadFAT(t *testing.T) {
	for kind, size := range map[string]int64{"fat12": 4 << 20, "fat16": 32 << 20, "fat32": 64 << 20} {
		t.Run(kind, func(t *testing.T) {
			files := tree(false)
			v, r := probeOne(t, image(t, kind, size, files))
			if v.FS != kind || v.Offset != 0 || v.Size != size {
				t.Fatalf("volume %+v", v)
			}
			src, err := Open(r, v)
			if err != nil {
				t.Fatal(err)
			}
			if src.Kind() != kind {
				t.Fatalf("kind %s", src.Kind())
			}
			checkTree(t, src, files)
			if st, err := src.Stat("DCIM/Camera/IMG_0001.jpg"); err != nil || st.ModTime().IsZero() {
				t.Fatalf("stat %v %v", st, err)
			}
		})
	}
}

func TestReadExt4(t *testing.T) {
	files := tree(true)
	v, r := probeOne(t, image(t, "ext4", 32<<20, files))
	if v.FS != "ext4" {
		t.Fatalf("volume %+v", v)
	}
	src, err := Open(r, v)
	if err != nil {
		t.Fatal(err)
	}
	delete(files, "lost+found/")
	checkTree(t, src, files)
}

func TestReadISO(t *testing.T) {
	files := tree(false)
	delete(files, "Android/.nomedia/hidden.jpg") // go-diskfs's ISO writer breaks Rock Ridge on dot-names
	v, r := probeOne(t, image(t, "iso9660", 16<<20, files))
	if v.FS != "iso9660" {
		t.Fatalf("volume %+v", v)
	}
	src, err := Open(r, v)
	if err != nil {
		t.Fatal(err)
	}
	checkTree(t, src, files)
}

func TestProbeMBR(t *testing.T) { testPartitioned(t, "mbr") }
func TestProbeGPT(t *testing.T) { testPartitioned(t, "gpt") }

func testPartitioned(t *testing.T, table string) {
	files := tree(false)
	r, size := openImage(t, partitioned(t, table, files))
	vs, err := Probe(r, size)
	if err != nil || len(vs) != 2 {
		t.Fatalf("Probe = %+v, %v", vs, err)
	}
	if vs[0].FS != "fat32" || vs[0].Offset != 2048*512 || vs[1].FS != "fat16" || vs[0].Index != 0 || vs[1].Index != 1 {
		t.Fatalf("volumes %+v", vs)
	}
	src, err := Open(r, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	checkTree(t, src, files)
	src2, err := Open(r, vs[1])
	if err != nil {
		t.Fatal(err)
	}
	checkTree(t, src2, map[string][]byte{"second.txt": fill(10, "two")})
}

func TestProbeSuperfloppy(t *testing.T) {
	v, _ := probeOne(t, image(t, "fat32", 64<<20, tree(false)))
	if v.Offset != 0 || v.Label != "FAT32" || v.Serial == "" {
		t.Fatalf("%+v", v)
	}
}

// mkfs.vfat -F 32 makes FAT32 volumes with fewer than 65,525 clusters on small cards. The boot
// sector is a FAT32 one (16-bit FAT size 0, no fixed root directory), which is how Linux and
// Windows tell; the cluster-count rule alone called it FAT16 and the volume couldn't be read.
func TestSmallFAT32KnownByBootSector(t *testing.T) {
	b, err := os.ReadFile(image(t, "fat32", 64<<20, tree(false)))
	if err != nil {
		t.Fatal(err)
	}
	bs := append([]byte(nil), b[:512]...)
	bs[13] = 64 // 64 sectors per cluster: ~2,000 clusters
	if k := fatKind(bs); k != "fat32" {
		t.Fatalf("fatKind = %q", k)
	}
}

// synthetic builds a 2 MiB image whose only content is a file-system signature.
func synthetic(off int, sig []byte) []byte {
	b := make([]byte, 2<<20)
	copy(b[off:], sig)
	return b
}

func TestProbeIdentifiesEachFS(t *testing.T) {
	ext := make([]byte, 2)
	binary.LittleEndian.PutUint16(ext, 0xEF53)
	cases := map[string][]byte{
		"exfat":   synthetic(3, []byte("EXFAT   ")),
		"ntfs":    synthetic(3, []byte("NTFS    ")),
		"ext4":    synthetic(1080, ext),
		"iso9660": synthetic(32769, []byte("CD001")),
		"apfs":    synthetic(32, []byte("NXSB")),
		"hfs+":    synthetic(1024, []byte("H+")),
	}
	for want, img := range cases {
		vs, err := Probe(bytes.NewReader(img), int64(len(img)))
		if err != nil || len(vs) != 1 || vs[0].FS != want {
			t.Errorf("%s: %+v %v", want, vs, err)
		}
	}
	vs, err := Probe(bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	if err != nil || len(vs) != 1 || vs[0].FS != "unknown" {
		t.Errorf("blank: %+v %v", vs, err)
	}
}

func TestAPFSReportsOSMount(t *testing.T) { testOSMountOnly(t, "apfs", synthetic(32, []byte("NXSB"))) }
func TestHFSPlusReportsOSMount(t *testing.T) {
	testOSMountOnly(t, "hfs+", synthetic(1024, []byte("H+")))
}

func testOSMountOnly(t *testing.T, fsName string, img []byte) {
	r := bytes.NewReader(img)
	vs, _ := Probe(r, int64(len(img)))
	if _, err := Open(r, vs[0]); !errors.Is(err, ErrMountWithOS) {
		t.Fatalf("%s Open err = %v", fsName, err)
	}
	for _, c := range Capabilities(&vs[0], nil) {
		if c.Enabled || !strings.Contains(c.Reason, "mount with the OS") {
			t.Fatalf("%s capability %+v", fsName, c)
		}
	}
}

func TestCapabilitiesTable(t *testing.T) {
	want := map[string][3]bool{ // Read, Undelete, SkipLive
		"fat12": {true, true, true}, "fat16": {true, true, true}, "fat32": {true, true, true},
		"exfat": {true, true, true}, "ntfs": {true, false, false},
		"ext4": {true, false, false}, "iso9660": {true, false, false},
		"apfs": {false, false, false}, "hfs+": {false, false, false}, "unknown": {false, false, false},
	}
	for fsName, w := range want {
		cs := Capabilities(&Volume{FS: fsName}, nil)
		if len(cs) != 3 || cs[0].Name != "Read" || cs[1].Name != "Undelete" || cs[2].Name != "SkipLive" {
			t.Fatalf("%s: %+v", fsName, cs)
		}
		for i, c := range cs {
			if c.Enabled != w[i] || (!c.Enabled && c.Reason == "") || len(c.Reason) > 60 {
				t.Errorf("%s %s: %+v", fsName, c.Name, c)
			}
		}
	}
	folder := Capabilities(nil, nil)
	if !folder[0].Enabled || folder[1].Enabled || !strings.Contains(folder[1].Reason, "admin") {
		t.Errorf("folder: %+v", folder)
	}
	if c := Capabilities(&Volume{FS: "ext4"}, nil)[1]; c.Reason != "ext4 wipes deleted file locations" {
		t.Errorf("ext4 undelete reason %q", c.Reason)
	}
}

func TestStorageIsReadOnly(t *testing.T) {
	s := newStorage(bytes.NewReader(make([]byte, 4096)), 4096)
	if _, err := s.Writable(); err == nil {
		t.Fatal("Writable() succeeded")
	}
	if _, err := s.Sys(); err == nil {
		t.Fatal("Sys() succeeded")
	}
	if n, err := s.ReadAt(make([]byte, 10), 4090); n != 6 || err != io.EOF {
		t.Fatalf("ReadAt past end: %d %v", n, err)
	}
}

func TestGarbageDoesNotPanic(t *testing.T) {
	img := make([]byte, 4<<20)
	for i := range img {
		img[i] = byte(i*131 + i>>9)
	}
	for _, sig := range [][]byte{nil, []byte("EXFAT   "), []byte("NTFS    ")} {
		b := append([]byte{}, img...)
		copy(b[3:], sig)
		b[510], b[511] = 0x55, 0xAA
		vs, _ := Probe(bytes.NewReader(b), int64(len(b)))
		for _, v := range vs {
			if src, err := Open(bytes.NewReader(b), v); err == nil {
				src.ReadDir(".")
			}
		}
	}
}

func FuzzProbe(f *testing.F) {
	f.Add(synthetic(3, []byte("EXFAT   "))[:4096])
	f.Add(synthetic(3, []byte("NTFS    "))[:4096])
	f.Fuzz(func(t *testing.T, b []byte) {
		vs, _ := Probe(bytes.NewReader(b), int64(len(b)))
		for _, v := range vs {
			if src, err := Open(bytes.NewReader(b), v); err == nil {
				src.ReadDir(".")
			}
		}
	})
}

func gunzipFixture(t *testing.T, name string) *bytes.Reader {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// TestReadNTFS uses go-ntfs's own sample volume (testdata/NOTICE).
func TestReadNTFS(t *testing.T) {
	r := gunzipFixture(t, "ntfs.img.gz")
	vs, err := Probe(r, r.Size())
	if err != nil || len(vs) != 1 || vs[0].FS != "ntfs" {
		t.Fatalf("%+v %v", vs, err)
	}
	src, err := Open(r, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	root, err := src.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range root {
		names[e.Name()] = true
		if strings.HasPrefix(e.Name(), "$") {
			t.Errorf("NTFS metadata file listed: %s", e.Name())
		}
	}
	if !names["Folder A"] {
		t.Fatalf("root = %v", names)
	}
	es, err := src.ReadDir("Folder A/Folder B")
	if err != nil {
		t.Fatal(err)
	}
	const doc = "Folder A/Folder B/Hello world text document.txt"
	found := false
	for _, e := range es {
		if strings.Contains(e.Name(), ":") {
			t.Errorf("alternate stream listed as a file: %s", e.Name())
		}
		found = found || e.Name() == "Hello world text document.txt"
	}
	if !found {
		t.Fatalf("listing %v", es)
	}
	ra, size, c, err := src.OpenAt(doc)
	if err != nil || size != 12 {
		t.Fatal(err, size)
	}
	defer c.Close()
	b := make([]byte, size)
	if n, _ := ra.ReadAt(b, 0); n != 12 || bytes.Count(b, []byte{0}) == 12 {
		t.Fatalf("content %q", b[:n])
	}
}
