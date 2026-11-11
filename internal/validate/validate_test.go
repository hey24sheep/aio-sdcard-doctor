package validate

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func testImage() image.Image {
	m := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			m.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), uint8(x ^ y), 255})
		}
	}
	return m
}

func enc(t *testing.T, f func(*bytes.Buffer) error) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := f(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func ftypBox(brand string) []byte { return box("ftyp", []byte(brand+"\x00\x00\x00\x00"+brand)) }

func pdfBytes() []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\nxref\n0 1\ntrailer\n<< /Root 1 0 R >>\nstartxref\n9\n%%EOF\n")
}

func zipBytes(t *testing.T) []byte {
	return enc(t, func(b *bytes.Buffer) error {
		w := zip.NewWriter(b)
		f, _ := w.Create("a.txt")
		f.Write([]byte(strings.Repeat("hello ", 100)))
		return w.Close()
	})
}

type fixture struct {
	ext  string
	data []byte
}

func fixtures(t *testing.T) []fixture {
	img := testImage()
	mp4 := append(append(ftypBox("isom"), box("moov", box("mvhd", make([]byte, 100)))...), box("mdat", make([]byte, 500))...)
	heic := append(ftypBox("heic"), box("meta", make([]byte, 60))...)
	heic = append(heic, box("mdat", bytes.Repeat([]byte{7}, 300))...)
	return []fixture{
		{".jpg", enc(t, func(b *bytes.Buffer) error { return jpeg.Encode(b, img, nil) })},
		{".png", enc(t, func(b *bytes.Buffer) error { return png.Encode(b, img) })},
		{".gif", enc(t, func(b *bytes.Buffer) error { return gif.Encode(b, img, nil) })},
		{".bmp", enc(t, func(b *bytes.Buffer) error { return bmp.Encode(b, img) })},
		{".tif", enc(t, func(b *bytes.Buffer) error { return tiff.Encode(b, img, nil) })},
		{".mp4", mp4},
		{".mov", mp4},
		{".heic", heic},
		{".pdf", pdfBytes()},
		{".zip", zipBytes(t)},
		{".docx", zipBytes(t)},
	}
}

func check(ext string, b []byte) (bool, string) {
	return Check(ext, bytes.NewReader(b), int64(len(b)))
}

func TestValidFiles(t *testing.T) {
	for _, f := range fixtures(t) {
		if ok, why := check(f.ext, f.data); !ok {
			t.Errorf("%s valid file rejected: %s", f.ext, why)
		}
	}
}

func TestTruncatedFiles(t *testing.T) {
	for _, f := range fixtures(t) {
		cut := f.data[:len(f.data)*2/3]
		if ok, _ := check(f.ext, cut); ok {
			t.Errorf("%s truncated to %d/%d bytes accepted", f.ext, len(cut), len(f.data))
		}
	}
}

func TestZeroFilledFiles(t *testing.T) {
	for _, f := range fixtures(t) {
		if ok, why := check(f.ext, make([]byte, len(f.data))); ok || why != "zeroed" {
			t.Errorf("%s zero-filled: ok=%v why=%q", f.ext, ok, why)
		}
	}
}

func TestTailZeroedImageRejected(t *testing.T) {
	j := enc(t, func(b *bytes.Buffer) error { return jpeg.Encode(b, testImage(), nil) })
	half := len(j) / 2
	damaged := append(append([]byte{}, j[:half]...), make([]byte, len(j)-half)...)
	if ok, _ := check(".jpg", damaged); ok {
		t.Error("jpeg with zeroed second half accepted")
	}
}

func TestEmptyAndOthers(t *testing.T) {
	if ok, why := check(".jpg", nil); ok || why != "empty" {
		t.Errorf("empty: %v %q", ok, why)
	}
	if ok, _ := check(".txt", []byte("plain text")); !ok {
		t.Error("text without an expected family should pass")
	}
	if ok, why := check(".jpg", append(ftypBox("heic"), make([]byte, 40)...)); ok || why != "mismatch" {
		t.Errorf("heic bytes named .jpg: %v %q", ok, why)
	}
}

func pngChunk(typ string, data []byte) []byte {
	b := make([]byte, 8, 12+len(data))
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	copy(b[4:], typ)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b[4:]))
}

func TestHugeDimensionsNotDecoded(t *testing.T) {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 20000)
	binary.BigEndian.PutUint32(ihdr[4:], 20000)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA → 1.6 GB if decoded
	b := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	b = append(b, pngChunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})...)
	b = append(b, pngChunk("IEND", nil)...)
	start := time.Now()
	ok, why := check(".png", b)
	if !ok || why != NotDecoded {
		t.Fatalf("huge png: ok=%v why=%q", ok, why)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v — decoded?", d)
	}
}

func FuzzCheck(f *testing.F) {
	for _, fx := range fixtures(&testing.T{}) {
		f.Add(fx.ext, fx.data)
	}
	f.Fuzz(func(t *testing.T, ext string, data []byte) {
		check(ext, data) // must not panic
	})
}

// Final review I3: an image too big to decode is passed with a note, not failed.
func TestHugeImagePassedWithNote(t *testing.T) {
	j := enc(t, func(b *bytes.Buffer) error { return jpeg.Encode(b, testImage(), nil) })
	i := bytes.Index(j, []byte{0xFF, 0xC0})
	copy(j[i+5:i+9], []byte{0x27, 0x10, 0x2E, 0xE0})
	ok, why := Check("jpg", bytes.NewReader(j), int64(len(j)))
	if !ok || !strings.Contains(why, "not decoded") {
		t.Fatalf("ok %v why %q", ok, why)
	}
}

// Adversarial pass D6: a video with data after its last box (Insta360 .insv files end with a
// trailer of their own) was refused as "truncated" or "trailing bytes". A trailer is fine; a box
// that really runs past the end is still truncation.
func TestVideoTrailerAllowed(t *testing.T) {
	mp4 := append(append(ftypBox("isom"), box("moov", box("mvhd", make([]byte, 100)))...), box("mdat", make([]byte, 500))...)
	trailer := append([]byte{0x00, 0x10, 0x00, 0x00, 0x8d, 0xb4, 0x2d, 0x69}, bytes.Repeat([]byte{0xC3}, 200)...)
	for name, b := range map[string][]byte{
		"binary trailer": append(append([]byte{}, mp4...), trailer...),
		"a few bytes":    append(append([]byte{}, mp4...), 1, 2, 3),
	} {
		if ok, why := check(".insv", b); !ok {
			t.Errorf("%s: %s", name, why)
		}
	}
	if ok, _ := check(".mp4", mp4[:len(mp4)-100]); ok {
		t.Error("an mp4 cut inside its mdat passed")
	}
}
