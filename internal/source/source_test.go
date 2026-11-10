package source

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
)

func TestSourceOSReadOnly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "DCIM"), 0o755)
	want := []byte("photo bytes")
	os.WriteFile(filepath.Join(dir, "DCIM", "a.jpg"), want, 0o644)
	src, err := OpenFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind() != "folder" {
		t.Fatalf("kind %q", src.Kind())
	}
	ents, err := src.ReadDir("DCIM")
	if err != nil || len(ents) != 1 || ents[0].Name() != "a.jpg" {
		t.Fatalf("ReadDir: %v %v", ents, err)
	}
	if st, err := src.Stat("DCIM/a.jpg"); err != nil || st.Size() != int64(len(want)) {
		t.Fatalf("Stat: %v %v", st, err)
	}
	r, size, c, err := src.OpenAt("DCIM/a.jpg")
	if err != nil || size != int64(len(want)) {
		t.Fatal(err, size)
	}
	if w, ok := r.(io.Writer); ok {
		if _, err := w.Write([]byte("X")); err == nil {
			t.Fatal("write through a source handle succeeded")
		}
	}
	got := make([]byte, size)
	r.ReadAt(got, 0)
	c.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("read %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "DCIM", "a.jpg")); !bytes.Equal(b, want) {
		t.Fatal("source file changed")
	}
	if _, _, _, err := src.OpenAt("../outside"); err == nil {
		t.Fatal("path outside the source opened")
	}
	if _, err := fs.ReadFile(src, "DCIM/a.jpg"); err != nil {
		t.Fatal(err)
	}
}

// strict fails unaligned reads, like a raw Windows disk handle.
type strict struct {
	data   []byte
	sector int64
	calls  int
}

func (s *strict) ReadAt(p []byte, off int64) (int, error) {
	s.calls++
	if off%s.sector != 0 || int64(len(p))%s.sector != 0 {
		return 0, fmt.Errorf("unaligned read off=%d len=%d", off, len(p))
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestAlignedReaderAt(t *testing.T) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	s := &strict{data: data, sector: 512}
	a := NewAligned(s, 512, int64(len(data)))
	cases := [][2]int{{0, 512}, {1, 10}, {511, 2}, {100, 5000}, {65000, 536}, {65530, 100}, {0, 65536}, {4096, 4096}}
	for _, c := range cases {
		buf := make([]byte, c[1])
		n, err := a.ReadAt(buf, int64(c[0]))
		end := min(c[0]+c[1], len(data))
		if n != end-c[0] || !bytes.Equal(buf[:n], data[c[0]:end]) {
			t.Fatalf("read %v: n=%d err=%v", c, n, err)
		}
		if end < c[0]+c[1] && err != io.EOF {
			t.Fatalf("read %v past end: err=%v", c, err)
		}
	}
	before := s.calls
	a.ReadAt(make([]byte, 8192), 8192)
	if s.calls != before+1 {
		t.Fatal("aligned read should pass straight through")
	}
}

// TestRawNeedsAdminKind opens a real disk read-only, so it only runs when asked for
// (AIO_REAL_DEVICE_TESTS=1, e.g. in CI): local runs never touch the user's drives.
func TestRawNeedsAdminKind(t *testing.T) {
	if os.Getenv("AIO_REAL_DEVICE_TESTS") != "1" {
		t.Skip("set AIO_REAL_DEVICE_TESTS=1 to open a real disk read-only")
	}
	if elevate.IsAdmin() {
		t.Skip("running elevated")
	}
	devs, err := drives.ListDevices()
	if err != nil || len(devs) == 0 {
		t.Skipf("no devices: %v", err)
	}
	_, _, c, err := OpenRawDevice(devs[0].Path)
	if err == nil {
		c.Close()
		t.Skipf("%s is readable without admin here", devs[0].Path)
	}
	if !errors.Is(err, ErrNeedsAdmin) {
		t.Fatalf("err = %v, want ErrNeedsAdmin", err)
	}
}

func TestOpenRawDeviceAcceptsImageFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "card.img")
	os.WriteFile(p, make([]byte, 8192), 0o644)
	r, size, c, err := OpenRawDevice(p)
	if err != nil || size != 8192 {
		t.Fatal(err, size)
	}
	defer c.Close()
	if _, err := r.ReadAt(make([]byte, 3), 5); err != nil {
		t.Fatal(err)
	}
}
