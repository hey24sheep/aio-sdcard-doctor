package carve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Adversarial pass: a stream (TS/M2TS/MTS, MPEG-PS, MP3, AAC, AMR, FLV, MKV of unknown size)
// ends where its data ends. Verify called a file that is exactly a whole number of frames
// "truncated", so intact camcorder clips, voice recordings and songs were never recovered.
// A file cut inside a frame is still truncated.
func TestVerifyAcceptsStreamsEndingAtEOF(t *testing.T) {
	for _, g := range generators {
		b := g.gen(t)
		if ok, why := Verify(g.ext, bytes.NewReader(b), int64(len(b))); !ok {
			t.Errorf("%s (%s) as generated: %s", g.ext, g.format, why)
		}
	}
	mp3 := genMP3(t)
	noTag := mp3[:len(mp3)-128] // no ID3v1 tag: most MP3s
	if ok, why := Verify("mp3", bytes.NewReader(noTag), int64(len(noTag))); !ok {
		t.Errorf("mp3 without ID3v1: %s", why)
	}
	mpg := genMPG(t)
	noEnd := mpg[:len(mpg)-4] // no end code: camcorder .MOD/.TOD often stop without one
	if ok, why := Verify("mpg", bytes.NewReader(noEnd), int64(len(noEnd))); !ok {
		t.Errorf("mpg without end code: %s", why)
	}
	for ext, b := range map[string][]byte{"mp3": noTag[:len(noTag)-100], "ts": genTS(t)[:188*20-50]} {
		if ok, _ := Verify(ext, bytes.NewReader(b), int64(len(b))); ok {
			t.Errorf("%s cut inside a frame passed", ext)
		}
	}
}

// pdfWith builds a minimal PDF whose startxref points at its xref table, padded to about n bytes.
func pdfWith(n int, linearized bool) []byte {
	head := "%PDF-1.5\n"
	if linearized {
		head += "1 0 obj <</Linearized 1 /L 0000000000 >> endobj\n"
	}
	b := []byte(head + "2 0 obj <<>> endobj\n")
	if linearized { // the first-page section ends with its own %%EOF
		b = append(b, "xref\n0 1\n0000000000 65535 f \ntrailer <<>>\nstartxref\n0\n%%EOF\n"...)
	}
	b = append(b, bytes.Repeat([]byte(" "), max(0, n-len(b)-80))...)
	x := len(b)
	b = append(b, fmt.Sprintf("xref\n0 1\n0000000000 65535 f \ntrailer <<>>\nstartxref\n%d\n%%%%EOF\n", x)...)
	if linearized {
		l := fmt.Sprintf("%010d", len(b))
		copy(b[bytes.Index(b, []byte("/L "))+3:], l)
	}
	return b
}

// Adversarial pass D3: a stale PDF head searched up to 1 GiB for any later %%EOF, swallowing the
// photos in between (never found) and passing an orphan tail off as a whole PDF; a linearized
// PDF over 8 MiB was cut at its first-page %%EOF and still called complete.
func TestPDFDoesNotSwallowFilesOrStopEarly(t *testing.T) {
	img := make([]byte, 4<<20)
	copy(img[512:], "%PDF-1.4\n1 0 obj <<>> endobj\n") // a deleted PDF's first cluster, nothing more
	j1, j2 := genJPEG(t), genJPEG(t)
	copy(img[64<<10:], j1)
	copy(img[1<<20:], j2)
	tail := pdfWith(3000, false)[2000:] // the end of some other PDF: startxref points nowhere here
	copy(img[3<<20:], tail)
	var hits []Hit
	Scan(context.Background(), bytes.NewReader(img), int64(len(img)), Options{Align: 512}, 0,
		func(h Hit) error { hits = append(hits, h); return nil }, nil)
	jpegs := 0
	for _, h := range hits {
		if h.Format == "jpeg" {
			jpegs++
		}
		if h.Format == "pdf" && h.Complete {
			t.Errorf("stale head + orphan tail taken for a whole PDF: %+v", h)
		}
	}
	if jpegs != 2 {
		t.Errorf("found %d of 2 photos behind the stale PDF head: %+v", jpegs, hits)
	}
	lin := pdfWith(9<<20, true)
	f := format(t, "pdf")
	buf := append(append([]byte{}, lin...), make([]byte, 4096)...)
	if n, ok, err := f.Size(bytes.NewReader(buf), 0, int64(len(buf))); err != nil || !ok || n != int64(len(lin)) {
		t.Errorf("linearized PDF: size %d complete %v err %v, want %d", n, ok, err, len(lin))
	}
}

// Adversarial pass (minor): two MP4s lying back to back became one find (the walk took the
// second ftyp as part of the first), and the second was never listed.
func TestAdjacentMP4sAreTwoFinds(t *testing.T) {
	a, b := genMP4(t), genMP4(t)
	n := (512 - len(a)%512) % 512 // a "free" box fills the first to a sector boundary: no gap
	if n > 0 && n < 8 {
		n += 512
	}
	if n > 0 {
		free := make([]byte, n)
		free[0], free[1], free[2], free[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		copy(free[4:], "free")
		a = append(a, free...)
	}
	img := append(append(append([]byte{}, a...), b...), make([]byte, 4096)...)
	var hits []Hit
	Scan(context.Background(), bytes.NewReader(img), int64(len(img)), Options{Align: 512}, 0,
		func(h Hit) error { hits = append(hits, h); return nil }, nil)
	finds := 0
	for _, h := range hits {
		if h.Format == "isobmff" {
			finds++
		}
	}
	if finds != 2 {
		t.Fatalf("%d MP4 finds, want 2: %+v", finds, hits)
	}
}

// Adversarial pass (minor): a modern APK keeps an "APK Signing Block" between its last file and
// the central directory, and the ZIP walk refused it, so every recovered APK was "not valid".
func TestAPKSigningBlockAccepted(t *testing.T) {
	z := genZIPStored(t)
	cd := bytes.Index(z, []byte("PK\x01\x02")) // where the central directory starts
	block := make([]byte, 8+32)                // size, one empty pair area, size, magic
	n := uint64(len(block) - 8)
	for i := 0; i < 8; i++ {
		block[i] = byte(n >> (8 * i))
	}
	copy(block[len(block)-16:], "APK Sig Block 42")
	for i := 0; i < 8; i++ {
		block[len(block)-24+i] = byte(n >> (8 * i))
	}
	apk := append(append(append([]byte{}, z[:cd]...), block...), z[cd:]...)
	// the end record's central-directory offset now points 40 bytes early: fix it up
	eocd := bytes.LastIndex(apk, []byte("PK\x05\x06"))
	off := uint32(cd + len(block))
	apk[eocd+16], apk[eocd+17], apk[eocd+18], apk[eocd+19] = byte(off), byte(off>>8), byte(off>>16), byte(off>>24)
	if ok, why := Verify("apk", bytes.NewReader(apk), int64(len(apk))); !ok {
		t.Fatalf("APK with a signing block: %s", why)
	}
}
