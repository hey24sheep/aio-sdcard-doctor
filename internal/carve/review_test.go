package carve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// streamReader is a virtual device: an endless run of 2048-byte MPEG-PS packs (camcorder MOD/TOD/
// VOB layout) of the given size. It counts reads and fails once a bound is passed, so a scan
// that re-walks the stream from every pack fails fast instead of running for hours.
type streamReader struct {
	pack       []byte
	size       int64
	calls, max int64
	bytes      int64
}

func newStream(size int64) *streamReader {
	p := make([]byte, 2048)
	copy(p, []byte{0, 0, 1, 0xBA, 0x44, 0, 4, 0, 4, 1, 1, 0x89, 0xC3, 0xF8})
	copy(p[14:], []byte{0, 0, 1, 0xE0, byte((2048 - 20) >> 8), byte((2048 - 20) & 0xFF)})
	return &streamReader{pack: p, size: size, max: 200_000}
}

func (s *streamReader) ReadAt(b []byte, off int64) (int, error) {
	s.calls++
	s.bytes += int64(len(b))
	if s.calls > s.max {
		return 0, errors.New("too many reads: the stream is being re-walked")
	}
	n := 0
	for n < len(b) && off+int64(n) < s.size {
		o := (off + int64(n)) % 2048
		n += copy(b[n:], s.pack[o:])
	}
	if int64(n) > s.size-off {
		n = int(max(0, s.size-off))
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// TestLongStreamBounded (review C1): a long stream without an end code is sized once; the scan
// reads it a bounded number of times and reports one find, not one per pack.
func TestLongStreamBounded(t *testing.T) {
	s := newStream(600 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var hits []Hit
	err := Scan(ctx, s, s.size, Options{}, 0, func(h Hit) error { hits = append(hits, h); return nil }, nil)
	if err != nil {
		t.Fatalf("scan: %v (%d reads, %d bytes)", err, s.calls, s.bytes)
	}
	if len(hits) != 1 || hits[0].Offset != 0 || hits[0].Complete {
		t.Fatalf("hits %+v", hits[:min(len(hits), 5)])
	}
	if s.bytes > 4*s.size {
		t.Fatalf("read %d bytes of a %d-byte stream", s.bytes, s.size)
	}
}

// TestStreamToEndOneFind (review C1): a TS stream running to the end of the image is one find.
func TestStreamToEndOneFind(t *testing.T) {
	var b []byte
	for i := 0; i < 40000; i++ {
		p := make([]byte, 192)
		p[4] = 0x47
		b = append(b, p...)
	}
	r := &countReader{r: bytes.NewReader(b)}
	var hits []Hit
	Scan(context.Background(), r, int64(len(b)), Options{}, 0, func(h Hit) error { hits = append(hits, h); return nil }, nil)
	if len(hits) != 1 || r.n > 4*int64(len(b)) {
		t.Fatalf("%d hits, %d bytes read of %d", len(hits), r.n, len(b))
	}
}

type countReader struct {
	r io.ReaderAt
	n int64
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	c.n += int64(len(p))
	return c.r.ReadAt(p, off)
}

// TestHeaderSizedHitDoesNotHideFiles (review I2): a stale MP4 header claiming 8 MiB doesn't hide a
// photo that sits inside the range it claims.
func TestHeaderSizedHitDoesNotHideFiles(t *testing.T) {
	buf := noise(10<<20, 3)
	hdr := append(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", make([]byte, 200))...)
	md := make([]byte, 8)
	md[0], md[1], md[2], md[3] = 0, 0x80, 0, 8 // mdat of 8 MiB
	copy(md[4:], "mdat")
	copy(buf, append(hdr, md...))
	jpg := genJPEG(t)
	copy(buf[1<<20:], jpg)
	hits := scanAll(t, buf, Options{})
	if !findHit(hits, placed{off: 1 << 20, data: jpg, ext: "jpg"}) {
		t.Fatalf("photo inside the stale header's range not found: %+v", hits)
	}
}

// TestReadErrorInsideFileReported (review I5): a find with an unreadable area inside it is reported
// (Complete=false, Unreadable), and an unreadable window is reported, not silently passed over.
func TestReadErrorInsideFileReported(t *testing.T) {
	jpg := genJPEG(t)
	body := noise(1<<20, 5) // entropy-coded data: no 0xFF bytes (those would be markers)
	for i := range body {
		if body[i] == 0xFF {
			body[i] = 0xFE
		}
	}
	big := append(append([]byte{}, jpg[:len(jpg)-2]...), body...)
	big = append(big, 0xFF, 0xD9)
	buf := append(big, noise(3<<20, 6)...)
	end := int64(len(big))
	r := &badReader{r: bytes.NewReader(buf), from: end - 4096, to: end - 3584}
	var hits []Hit
	var bad [][2]int64
	err := Scan(context.Background(), r, int64(len(buf)), Options{Unreadable: func(off, n int64) { bad = append(bad, [2]int64{off, n}) }}, 0,
		func(h Hit) error { hits = append(hits, h); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Offset != 0 || hits[0].Complete || !hits[0].Unreadable {
		t.Fatalf("hits %+v", hits)
	}
	if len(bad) == 0 {
		t.Fatal("unreadable area not reported")
	}
}

// TestSizerPanicIsContained (review M3): a sizer bug on crafted data doesn't take down the scan.
func TestSizerPanicIsContained(t *testing.T) {
	old := Registry
	defer func() { Registry = old }()
	Registry = append([]Format{{Name: "boom", Ext: "x", Magics: []Magic{m(0, "BOOM")},
		Size: func(io.ReaderAt, int64, int64) (int64, bool, error) { panic("crafted") }}}, old...)
	buf := noise(1<<20, 1)
	copy(buf[4096:], "BOOM")
	jpg := genJPEG(t)
	copy(buf[8192:], jpg)
	if !findHit(scanAll(t, buf, Options{}), placed{off: 8192, data: jpg, ext: "jpg"}) {
		t.Fatal("scan did not continue after a sizer panic")
	}
}

// TestVerifyStructure (review I1): a known header followed by garbage is not a valid file.
func TestVerifyStructure(t *testing.T) {
	mp3 := genMP3(t)
	bad := append(append([]byte{}, mp3[:30]...), noise(5000, 2)...)
	if ok, why := Verify("mp3", bytes.NewReader(bad), int64(len(bad))); ok || why == "" {
		t.Fatalf("mp3 header + noise: %v %q", ok, why)
	}
	if ok, why := Verify("mp3", bytes.NewReader(mp3), int64(len(mp3))); !ok {
		t.Fatalf("valid mp3 rejected: %q", why)
	}
	withTrailer := append(append([]byte{}, genJPEG(t)...), make([]byte, 300)...) // phones append data after the end marker
	if ok, why := Verify("jpg", bytes.NewReader(withTrailer), int64(len(withTrailer))); !ok {
		t.Fatalf("jpeg with trailer rejected: %q", why)
	}
	if ok, _ := Verify("txt", bytes.NewReader([]byte("plain text")), 10); !ok {
		t.Fatal("unknown content rejected")
	}
}
