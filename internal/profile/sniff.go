package profile

import (
	"bytes"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// sig is a gap-table entry for formats mimetype does not tell apart.
type sig struct {
	off   int
	magic string
	cat   Category
	ext   string
}

// refinements are checked before mimetype because they are more specific than what it reports
// (TIFF-based camera RAWs, Canon CR3 inside ISO-BMFF, Fuji RAF).
var refinements = []sig{
	{0, "FUJIFILMCCD-RAW", Photos, ".raf"},
	{0, "IIRO", Photos, ".orf"},
	{0, "IIU\x00", Photos, ".rw2"},
	{4, "ftypcrx ", Photos, ".cr3"},
}

func at(b []byte, off int, magic string) bool {
	return len(b) >= off+len(magic) && string(b[off:off+len(magic)]) == magic
}

// Sniff identifies a file from its first bytes (≥ 12 needed). Used for files without a usable
// name (LOST.DIR, FOUND.NNN fragments).
func Sniff(head []byte) (Category, string, bool) {
	if len(head) < 12 || allZero(head) {
		return "", "", false
	}
	for _, s := range refinements {
		if at(head, s.off, s.magic) {
			return s.cat, s.ext, true
		}
	}
	m := mimetype.Detect(head)
	if !strings.HasPrefix(m.String(), "text/") && !m.Is("application/octet-stream") {
		if c, ok := ByExt(m.Extension()); ok {
			return c, m.Extension(), true
		}
	}
	switch tsKind(head) {
	case ".ts":
		return Videos, ".ts", true
	case ".m2ts":
		return Videos, ".m2ts", true
	}
	return "", "", false
}

// tsKind detects MPEG transport streams: sync byte 0x47 every 188 bytes (.ts) or every 192
// bytes after a 4-byte timecode (.mts/.m2ts).
func tsKind(b []byte) string {
	if len(b) > 188 && b[0] == 0x47 && b[188] == 0x47 {
		return ".ts"
	}
	if len(b) > 196 && b[4] == 0x47 && b[196] == 0x47 {
		return ".m2ts"
	}
	return ""
}

func allZero(b []byte) bool { return len(bytes.Trim(b, "\x00")) == 0 }
