// Package validate checks that a recovered file is complete and decodable before it is written,
// so damaged undeletes and carves are flagged instead of saved. It runs in the worker process.
package validate

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"github.com/abema/go-mp4"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// MaxPixels is the largest image we fully decode (100 MP); larger ones pass with NotDecoded.
const MaxPixels = 100_000_000

// NotDecoded is the note on an image too big to decode: ok, but only its header was checked.
const NotDecoded = "image over 100 MP: header checked, picture not decoded"

// maxBoxes bounds the ISO-BMFF top-level walk.
const maxBoxes = 10_000

var (
	imageExts   = set("jpg jpeg jfif png gif bmp webp tif tiff")
	videoExts   = set("mp4 mov m4v 3gp 3g2 m4a cr3 insv")
	heifExts    = set("heic heif avif")
	zipExts     = set("zip docx xlsx pptx odt ods odp epub")
	familyNames = map[string]string{"jpg": "jpeg", "jfif": "jpeg", "tif": "tiff"}
)

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(s) {
		m[e] = true
	}
	return m
}

// Check reports whether the file looks complete for its extension, with a short reason if not
// ("truncated jpeg", "missing moov box", …). An ok file may carry a note (NotDecoded).
func Check(ext string, r io.ReaderAt, size int64) (ok bool, reason string) {
	defer func() {
		if p := recover(); p != nil {
			ok, reason = false, fmt.Sprintf("decoder crashed: %v", p)
		}
	}()
	e := strings.ToLower(strings.TrimPrefix(ext, "."))
	head := make([]byte, min(size, 512))
	n, _ := r.ReadAt(head, 0)
	switch hc := profile.HeaderCheck(ext, head[:n]); hc {
	case "empty", "zeroed", "mismatch":
		return false, hc
	}
	sr := io.NewSectionReader(r, 0, size)
	switch {
	case imageExts[e]:
		return checkImage(e, sr)
	case videoExts[e]:
		return checkBMFF(sr, size, "moov")
	case heifExts[e]:
		return checkBMFF(sr, size, "meta")
	case e == "pdf":
		return checkPDF(r, size)
	case zipExts[e]:
		if _, err := zip.NewReader(r, size); err != nil {
			return false, "zip central directory unreadable"
		}
	}
	return true, ""
}

func checkImage(e string, sr *io.SectionReader) (bool, string) {
	name := e
	if f, ok := familyNames[e]; ok {
		name = f
	}
	cfg, _, err := image.DecodeConfig(sr)
	if err != nil {
		return false, "not a valid " + name
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return true, NotDecoded
	}
	if _, err := sr.Seek(0, io.SeekStart); err != nil {
		return false, err.Error()
	}
	if _, _, err := image.Decode(sr); err != nil {
		return false, "truncated or corrupt " + name
	}
	return true, ""
}

// checkBMFF walks the top-level ISO-BMFF boxes: they must hold need, and none may run past the
// end. Data after the last box is a trailer (Insta360 .insv files end with one) and is fine; a
// "box" whose type isn't printable is where such a trailer starts.
func checkBMFF(sr *io.SectionReader, size int64, need string) (bool, string) {
	var last uint64 // end of the last whole box
	found, boxes, overrun := false, 0, false
	errTrailer := errors.New("trailer")
	_, err := mp4.ReadBoxStructure(sr, func(h *mp4.ReadHandle) (interface{}, error) {
		if boxes++; boxes > maxBoxes {
			return nil, fmt.Errorf("too many boxes")
		}
		bi := h.BoxInfo
		if !printableType(bi.Type.String()) {
			return nil, errTrailer
		}
		if bi.Type.String() == need {
			found = true
		}
		end := bi.Offset + bi.Size
		if bi.ExtendToEOF {
			end = uint64(size)
		}
		if end > uint64(size) {
			overrun = true
			return nil, errTrailer
		}
		last = end
		return nil, nil
	})
	switch {
	case overrun:
		return false, "truncated (box runs past end of file)"
	case err != nil && !errors.Is(err, errTrailer) && last == 0:
		return false, "box structure unreadable"
	case !found:
		return false, "missing " + need + " box"
	}
	return true, "" // whatever follows the last whole box is a trailer
}

// printableType: real box types are four printable characters ("moov", "mdat", "©xyz").
func printableType(t string) bool {
	if len(t) != 4 {
		return false
	}
	for _, c := range []byte(t) {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func checkPDF(r io.ReaderAt, size int64) (bool, string) {
	head := make([]byte, 5)
	if _, err := r.ReadAt(head, 0); err != nil || string(head) != "%PDF-" {
		return false, "missing %PDF header"
	}
	tail := make([]byte, min(size, 1024))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil && err != io.EOF {
		return false, "unreadable tail"
	}
	if !bytes.Contains(tail, []byte("%%EOF")) || !bytes.Contains(tail, []byte("startxref")) {
		return false, "truncated pdf (no %%EOF/startxref)"
	}
	return true, ""
}
