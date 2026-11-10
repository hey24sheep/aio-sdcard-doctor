// Package carve is our own PhotoRec-style engine: it finds files on a device or image by their
// signatures, at sector-aligned offsets, and sizes each one by walking its structure. A hit whose
// structure ends early is reported with Complete=false and is never written by the caller.
package carve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// Magic is a signature at Off bytes from the start of a file. A format matches if any of its
// magics match; its Size function rejects look-alikes.
type Magic struct {
	Off   int
	Bytes []byte
}

// Format is one carvable file format.
type Format struct {
	Name, Ext string
	Cat       profile.Category
	Magics    []Magic
	MaxSize   int64
	// Size walks the structure from off (reading at most limit bytes) and returns the file length;
	// complete=false when the structure ends early (truncated) — such hits are flagged, not extracted.
	Size func(r io.ReaderAt, off, limit int64) (n int64, complete bool, err error)
	// Sub, when set, names the extension from the file's content (docx in a ZIP, nef in a TIFF, …).
	Sub func(r io.ReaderAt, off, n int64) string
	// HeaderSized: the end comes from sizes in the header, not from walking the data, so a stale
	// header could claim space that now holds other files; Scan keeps looking inside it.
	HeaderSized bool
}

// Hit is one file found by Scan.
type Hit struct {
	Offset, Size int64
	Format       string
	Ext          string
	Cat          profile.Category
	Complete     bool
	Unreadable   bool // part of it could not be read (it is also not Complete)
}

// Options for Scan.
type Options struct {
	Align   int        // 512 default; 1 = thorough (every byte offset)
	Skip    [][2]int64 // regions not to look in (live files), [start, end)
	Formats []string   // format names to look for; empty = all
	// Unreadable is told about every area that could not be read (and so was not searched).
	Unreadable func(off, n int64)
}

const (
	kib = int64(1) << 10
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// Registry is every built-in format, in match priority order.
var Registry = []Format{
	jpegFormat, pngFormat, gifFormat, bmpFormat, tiffFormat, rafFormat,
	riffFormat, aiffFormat, bmffFormat, ebmlFormat, tsFormat, psFormat, flvFormat, asfFormat,
	mp3Format, aacFormat, amrFormat, flacFormat, oggFormat, midiFormat,
	pdfFormat, zipFormat, sevenZipFormat, rarFormat, gzipFormat, sqliteFormat, ole2Format, rtfFormat,
}

// Unsupported lists formats deep scan cannot find, with the reason shown to the user.
var Unsupported = map[string]string{
	"crypt12": "encrypted, no recognisable header",
	"crypt14": "encrypted, no recognisable header",
	"crypt15": "encrypted, no recognisable header",
	"mxf":     "pro video, not yet supported",
	"braw":    "pro video, not yet supported",
	"r3d":     "pro video, not yet supported",
	"txt":     "no file header to find",
	"csv":     "no file header to find",
	"vcf":     "no file header to find",
}

var (
	errTrunc    = errors.New("truncated")
	errBad      = errors.New("not this format")
	errSteps    = errors.New("structure too long")
	errNotFound = errors.New("not found")
)

// maxSteps bounds every structure walk.
const maxSteps = 1 << 21

// rd reads a candidate file: offsets are relative to its start; reads past limit are errTrunc.
type rd struct {
	r      io.ReaderAt
	base   int64
	limit  int64
	seen   int64 // furthest byte read
	steps  int
	win    []byte // read-ahead window: a walk's many small reads become few large ones
	winOff int64
	winLen int64 // next window size: starts small (most candidates fail early), doubles to 1 MiB
}

func (x *rd) bytes(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 {
		return nil, errBad
	}
	if off+int64(n) > x.limit {
		if off < x.limit {
			x.seen = max(x.seen, x.limit)
		}
		return nil, errTrunc
	}
	if n <= 64*1024 {
		if off < x.winOff || off+int64(n) > x.winOff+int64(len(x.win)) {
			x.winLen = min(max(x.winLen*2, 4096), window)
			b := make([]byte, min(x.winLen, x.limit-off))
			got, _ := x.r.ReadAt(b, x.base+off) // a short window falls back to an exact read below
			x.win, x.winOff = b[:got], off
		}
		if end := off + int64(n); end <= x.winOff+int64(len(x.win)) {
			x.seen = max(x.seen, end)
			i := off - x.winOff
			return x.win[i : i+int64(n) : i+int64(n)], nil
		}
	}
	b := make([]byte, n)
	m, err := x.r.ReadAt(b, x.base+off)
	if m < n {
		if err == nil || errors.Is(err, io.EOF) {
			return nil, errTrunc
		}
		return nil, err
	}
	x.seen = max(x.seen, off+int64(n))
	return b, nil
}

func (x *rd) step() error {
	x.steps++
	if x.steps > maxSteps {
		return errSteps
	}
	return nil
}

// find returns the offset of the first pat at or after from, searching up to stop (exclusive).
func (x *rd) find(from, stop int64, pats ...[]byte) (int64, []byte, error) {
	const chunk = 64 * 1024
	long := 0
	for _, p := range pats {
		long = max(long, len(p))
	}
	for pos := from; pos < stop; pos += chunk {
		if err := x.step(); err != nil {
			return 0, nil, err
		}
		n := min(int64(chunk+long-1), x.limit-pos)
		if n < int64(long) {
			break
		}
		b, err := x.bytes(pos, int(n))
		if err != nil {
			return 0, nil, err
		}
		best, which := -1, []byte(nil)
		for _, p := range pats {
			if i := bytes.Index(b, p); i >= 0 && i < chunk && (best < 0 || i < best) {
				best, which = i, p
			}
		}
		if best >= 0 {
			return pos + int64(best), which, nil
		}
	}
	if stop >= x.limit {
		x.seen = max(x.seen, x.limit)
		return 0, nil, errTrunc
	}
	return 0, nil, errNotFound
}

// sizer adapts a structure walk to Format.Size: errTrunc → incomplete with what was seen.
func sizer(walk func(x *rd) (int64, error)) func(io.ReaderAt, int64, int64) (int64, bool, error) {
	return func(r io.ReaderAt, off, limit int64) (int64, bool, error) {
		x := &rd{r: r, base: off, limit: limit}
		n, err := walk(x)
		switch {
		case err == nil:
			if n <= 0 || n > limit {
				return 0, false, errBad
			}
			return n, true, nil
		case errors.Is(err, errTrunc) || errors.Is(err, errSteps):
			if x.seen <= 0 {
				return 0, false, errBad
			}
			return min(x.seen, limit), false, nil
		case errors.Is(err, errBad) || errors.Is(err, errNotFound):
			return 0, false, errBad
		default: // the structure ran into an area that can't be read
			if x.seen <= 0 {
				return 0, false, err
			}
			return min(x.seen, limit), false, fmt.Errorf("%w: %v", ErrUnreadable, err)
		}
	}
}

// ErrUnreadable: Size reached an area that can't be read; n is what was walked before it.
var ErrUnreadable = errors.New("unreadable area inside the file")

func le16(b []byte) int64 { return int64(b[0]) | int64(b[1])<<8 }
func le32(b []byte) int64 { return le16(b) | le16(b[2:])<<16 }
func le64(b []byte) uint64 {
	return uint64(le32(b)) | uint64(le32(b[4:]))<<32
}
func be16(b []byte) int64 { return int64(b[0])<<8 | int64(b[1]) }
func be32(b []byte) int64 { return be16(b)<<16 | be16(b[2:]) }
func be64(b []byte) uint64 {
	return uint64(be32(b))<<32 | uint64(be32(b[4:]))
}

func m(off int, s string) Magic { return Magic{Off: off, Bytes: []byte(s)} }

// extFormats maps a file extension to the format whose sizer can check it.
var extFormats = map[string]string{"jpg": "jpeg", "jpeg": "jpeg", "jfif": "jpeg", "png": "png", "gif": "gif",
	"bmp": "bmp", "tif": "tiff", "tiff": "tiff", "nef": "tiff", "cr2": "tiff", "arw": "tiff", "dng": "tiff",
	"orf": "tiff", "rw2": "tiff", "pef": "tiff", "srw": "tiff", "raf": "raf", "webp": "riff", "wav": "riff",
	"avi": "riff", "aif": "aiff", "aiff": "aiff", "mp4": "isobmff", "mov": "isobmff", "m4v": "isobmff",
	"m4a": "isobmff", "3gp": "isobmff", "heic": "isobmff", "heif": "isobmff", "avif": "isobmff", "cr3": "isobmff",
	"insv": "isobmff", "mkv": "ebml", "webm": "ebml", "ts": "mpegts", "mts": "mpegts", "m2ts": "mpegts",
	"mpg": "mpegps", "mpeg": "mpegps", "vob": "mpegps", "mod": "mpegps", "tod": "mpegps", "flv": "flv",
	"wmv": "asf", "wma": "asf", "mp3": "mp3", "aac": "aac", "amr": "amr", "flac": "flac", "ogg": "ogg",
	"opus": "ogg", "oga": "ogg", "mid": "midi", "midi": "midi", "pdf": "pdf", "zip": "zip", "docx": "zip",
	"xlsx": "zip", "pptx": "zip", "odt": "zip", "ods": "zip", "odp": "zip", "epub": "zip", "apk": "zip",
	"7z": "7z", "rar": "rar", "gz": "gzip", "tgz": "gzip", "sqlite": "sqlite", "db": "sqlite", "doc": "ole2",
	"xls": "ole2", "ppt": "ole2", "msg": "ole2", "rtf": "rtf"}

// Verify checks a whole file's structure with the carver's sizer for its extension: a file that
// starts like its format must walk to a complete end within its size (data after the end, like a
// phone's trailer after a JPEG, is fine). Extensions without a sizer, or content that doesn't
// start like the format (the header check reports that), pass.
func Verify(ext string, r io.ReaderAt, size int64) (bool, string) {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	name, ok := extFormats[ext]
	if !ok || size <= 0 {
		return true, ""
	}
	for _, f := range Registry {
		if f.Name != name {
			continue
		}
		head := make([]byte, 64)
		n, _ := r.ReadAt(head, 0)
		if !matches(f, head[:n]) {
			return true, ""
		}
		got, complete, err := safeSize(f, r, 0, size)
		switch {
		case err == nil && complete && got <= size:
			return true, ""
		case got > 0 || errors.Is(err, ErrUnreadable):
			return false, "truncated or damaged " + ext + " (its structure ends early)"
		}
		return false, "not a valid " + ext + " (its structure does not hold together)"
	}
	return true, ""
}

// safeSize contains a sizer bug on crafted data: a panic is a non-match, never a crash.
func safeSize(f Format, r io.ReaderAt, off, limit int64) (n int64, complete bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			n, complete, err = 0, false, fmt.Errorf("%s sizer: %v", f.Name, v)
		}
	}()
	return f.Size(r, off, limit)
}

func safeSub(f Format, r io.ReaderAt, off, n int64) (ext string) {
	defer func() {
		if recover() != nil {
			ext = ""
		}
	}()
	return f.Sub(r, off, n)
}
