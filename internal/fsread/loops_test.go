package fsread

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"
)

// realImage is one of testdata/realfs, unpacked.
func realImage(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "realfs", name+".img.gz"))
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
	return b
}

// walkImage opens the first volume of img and walks it: files seen per base name, and the
// folders that failed with their errors.
func walkImage(t *testing.T, img []byte) (names map[string]int, failed map[string]error) {
	t.Helper()
	r := bytes.NewReader(img)
	vs, err := Probe(r, r.Size())
	if err != nil || len(vs) == 0 {
		t.Fatalf("probe: %v %v", vs, err)
	}
	src, err := Open(r, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	names, failed = map[string]int{}, map[string]error{}
	within(t, 30*time.Second, func() {
		fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				failed[p] = err
			case !d.IsDir():
				names[path.Base(p)]++
			}
			return nil
		})
	})
	return names, failed
}

// checkLoopReported: every file is seen once and the folder that closes the loop is reported
// with errDirLoop.
func checkLoopReported(t *testing.T, names map[string]int, failed map[string]error, at string) {
	t.Helper()
	for n, k := range names {
		if k != 1 {
			t.Errorf("%s seen %d times", n, k)
		}
	}
	if err := failed[at]; !errors.Is(err, errDirLoop) || len(failed) != 1 {
		t.Errorf("failed folders %v; want only %s with errDirLoop", failed, at)
	}
}

// exfatSetOf finds the file entry set (0x85, 0xC0, 0xC1…) whose one-letter name is c.
func exfatSetOf(t *testing.T, img []byte, c byte) int {
	t.Helper()
	le := binary.LittleEndian
	for i := 0; i+96 <= len(img); i += 32 {
		if img[i] == 0x85 && img[i+32] == 0xC0 && img[i+64] == 0xC1 && img[i+35] == 1 && le.Uint16(img[i+66:]) == uint16(c) {
			return i
		}
	}
	t.Fatalf("no exFAT entry set named %q", c)
	return 0
}

// exfatFixChecksum recomputes an exFAT entry set's checksum after it was changed.
func exfatFixChecksum(img []byte, set int) {
	n := int(img[set+1]) + 1
	var sum uint16
	for k := 0; k < n*32; k++ {
		if k == 2 || k == 3 {
			continue
		}
		sum = (sum<<15 | sum>>1) + uint16(img[set+k])
	}
	binary.LittleEndian.PutUint16(img[set+2:], sum)
}

// OR3: an exFAT folder record pointing back at the root repeated the whole tree to the depth
// cap (×64 copies of every file). The folder that closes the loop is reported instead.
func TestExFATFolderPointingAtRootIsReported(t *testing.T) {
	img := realImage(t, "exfat")
	le := binary.LittleEndian
	cs := int64(1) << img[108] << img[109]
	set := exfatSetOf(t, img, 'd')
	se := img[set+32:] // the stream extension: flags, sizes, first cluster
	le.PutUint32(se[20:], le.Uint32(img[96:]))
	le.PutUint64(se[24:], uint64(cs))
	le.PutUint64(se[8:], uint64(cs))
	se[1] &^= 2 // follow the FAT chain, as the root does
	exfatFixChecksum(img, set)
	names, failed := walkImage(t, img)
	checkLoopReported(t, names, failed, "a/b/c/d")
}

// ext4dirent finds the directory record (inode, rec_len, name_len, type 2 = folder) of name.
func ext4dirent(t *testing.T, img []byte, name string) int {
	t.Helper()
	pat := append([]byte{byte(len(name)), 2}, name...)
	for i := 0; ; {
		j := bytes.Index(img[i:], pat)
		if j < 0 {
			t.Fatalf("no ext4 folder record %q", name)
		}
		if k := i + j - 6; k%4 == 0 {
			return k
		}
		i += j + 1
	}
}

// OR3 on ext4: folder d pointing at the root inode.
func TestExt4FolderPointingAtRootIsReported(t *testing.T) {
	img := realImage(t, "ext4")
	binary.LittleEndian.PutUint32(img[ext4dirent(t, img, "d"):], 2)
	names, failed := walkImage(t, img)
	checkLoopReported(t, names, failed, "a/b/c/d")
}

// ntfsRecord finds the MFT record (1024 bytes, "FILE") of a folder holding name in its index
// ($I30), and where name sits in it.
func ntfsRecord(t *testing.T, img []byte, name string) (rec, at int) {
	t.Helper()
	n := utf16le(name)
	for off := 0; off+1024 <= len(img); off += 1024 {
		m := img[off : off+1024]
		if i := bytes.Index(m, n); bytes.HasPrefix(m, []byte("FILE")) && i >= 0 && bytes.Contains(m, utf16le("$I30")) {
			return off, i
		}
	}
	t.Fatalf("no folder record holding %q", name)
	return 0, 0
}

// OR3 on NTFS: Folder B's index entry for its file made to point back at Folder A, under that
// name.
func TestNTFSFolderPointingAtParentIsReported(t *testing.T) {
	r := gunzipFixture(t, "ntfs.img.gz")
	img := make([]byte, r.Size())
	r.ReadAt(img, 0)
	le := binary.LittleEndian
	a, _ := ntfsRecord(t, img, "Folder A")
	b, at := ntfsRecord(t, img, "Hello world text document.txt")
	// the index entry: file reference (6-byte record number, 2-byte sequence), lengths, flags, and
	// the key: a copy of the file name attribute, whose name length is at +64 and name at +66
	ent := at - 16 - 66
	le.PutUint32(img[b+ent:], le.Uint32(img[a+0x2C:])) // Folder A's record number
	le.PutUint16(img[b+ent+4:], 0)
	le.PutUint16(img[b+ent+6:], le.Uint16(img[a+0x10:])) // and its sequence number
	img[b+at-2] = byte(len("Folder A"))
	copy(img[b+at:], utf16le("Folder A"))
	names, failed := walkImage(t, img)
	checkLoopReported(t, names, failed, "Folder A/Folder B/Folder A")
}
