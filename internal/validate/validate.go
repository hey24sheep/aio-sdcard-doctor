// Package validate checks that a recovered file is complete and decodable before it is written,
// so damaged undeletes and carves are flagged instead of saved. It runs in the worker process.
package validate

import (
	"archive/zip"
	"bytes"
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

// checkBMFF walks the top-level ISO-BMFF boxes: they must tile the file exactly and include need.
func checkBMFF(sr *io.SectionReader, size int64, need string) (bool, string) {
	var end uint64
	found, boxes := false, 0
	_, err := mp4.ReadBoxStructure(sr, func(h *mp4.ReadHandle) (interface{}, error) {
		if boxes++; boxes > maxBoxes {
			return nil, fmt.Errorf("too many boxes")
		}
		bi := h.BoxInfo
		if bi.Type.String() == need {
			found = true
		}
		if bi.ExtendToEOF {
			end = uint64(size)
		} else {
			end = bi.Offset + bi.Size
		}
		return nil, nil
	})
	switch {
	case end > uint64(size):
		return false, "truncated (box runs past end of file)"
	case err != nil:
		return false, "box structure unreadable"
	case !found:
		return false, "missing " + need + " box"
	case end != uint64(size):
		return false, "trailing bytes after last box"
	}
	return true, ""
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
