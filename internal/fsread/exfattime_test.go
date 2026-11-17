package fsread

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// exfatSetNamed finds the file entry set (0x85, 0xC0, 0xC1…) of a name of up to 15 characters.
func exfatSetNamed(t *testing.T, img []byte, name string) int {
	t.Helper()
	n := utf16le(name)
	for i := 0; i+96 <= len(img); i += 32 {
		if img[i] == 0x85 && img[i+32] == 0xC0 && img[i+64] == 0xC1 && int(img[i+35]) == len(name) && bytes.HasPrefix(img[i+66:i+96], n) {
			return i
		}
	}
	t.Fatalf("no exFAT entry set named %q", name)
	return 0
}

// OC5: an exFAT time stored without a UTC offset is the camera's wall clock, which the OS shows
// as local time (as FAT's). libxfat labels it UTC, so a copy made from the mounted card and one
// made from the raw card disagreed by the time zone, and resume read every file again.
func TestExFATTimeWithoutOffsetIsLocal(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("test", 5*3600+1800) // a zone that isn't UTC
	t.Cleanup(func() { time.Local = old })

	img := realImage(t, "exfat")
	set := exfatSetNamed(t, img, "IMG_0001.jpg")
	img[set+23] = 0 // modified time: no UTC offset recorded
	exfatFixChecksum(img, set)
	packed := binary.LittleEndian.Uint32(img[set+12:])
	want := time.Date(int(packed>>25)+1980, time.Month(packed>>21&15), int(packed>>16&31),
		int(packed>>11&31), int(packed>>5&63), int(packed&31)*2, 0, time.Local).Add(time.Duration(img[set+21]) * 10 * time.Millisecond)

	r := bytes.NewReader(img)
	vs, err := Probe(r, r.Size())
	if err != nil || len(vs) == 0 {
		t.Fatal(vs, err)
	}
	src, err := Open(r, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(src, "DCIM/Camera/IMG_0001.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(want) {
		t.Errorf("mtime %v, want the stored wall clock in local time %v", fi.ModTime(), want)
	}
}

// SR2: the exFAT root folder always follows the FAT chain; one zeroed or bad FAT entry for its
// cluster made the whole card unreadable (and undelete impossible) though the folder is intact.
func TestExFATRootWithDamagedFATEntryIsRead(t *testing.T) {
	img := realImage(t, "exfat")
	le := binary.LittleEndian
	bps := int64(1) << img[108]
	fatOff := int64(le.Uint32(img[80:])) * bps
	root := int64(le.Uint32(img[96:]))
	at := fatOff + root*4
	t.Run("unreadable", func(t *testing.T) {
		sec := at / bps * bps
		checkExFATRoot(t, img, badAreas{bytes.NewReader(img), [][2]int64{{sec, sec + bps}}})
	})
	t.Run("zeroed", func(t *testing.T) { // as a lost FAT sector reads in an image
		le.PutUint32(img[at:], 0)
		checkExFATRoot(t, img, bytes.NewReader(img))
	})
}

func checkExFATRoot(t *testing.T, img []byte, r io.ReaderAt) {
	t.Helper()
	vs, err := Probe(r, int64(len(img)))
	if err != nil || len(vs) == 0 {
		t.Fatal(vs, err)
	}
	src, err := Open(r, vs[0])
	if err != nil {
		t.Fatal(err)
	}
	f, err := src.Open("DCIM/Camera/IMG_0001.jpg")
	if err != nil {
		t.Fatalf("open a file under the root: %v", err)
	}
	b := make([]byte, 6000)
	n, _ := f.Read(b)
	f.Close()
	if !bytes.Equal(b[:n], fill(5000, "jpg1")) {
		t.Errorf("read %d bytes, want the file", n)
	}
	deleted := 0
	if err := src.(interface {
		Deleted(context.Context, func(source.Deleted) error) error
	}).Deleted(context.Background(), func(source.Deleted) error { deleted++; return nil }); err != nil || deleted == 0 {
		t.Errorf("deleted files: %d, %v", deleted, err)
	}
}
