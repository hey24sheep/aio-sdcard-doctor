package carve

import (
	"compress/flate"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"unicode/utf16"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// PDF: a linearized file's own length (/L), else the last %%EOF before the next %PDF- (or 8 MiB
// without another %%EOF, or the cap); the end's startxref must point at an xref table or object
// inside it, or it is the tail of some other PDF. Scan keeps looking inside its range.
var pdfFormat = Format{Name: "pdf", Ext: "pdf", Cat: profile.Documents, MaxSize: 1 * gib, HeaderSized: true,
	Magics: []Magic{m(0, "%PDF-")}, Size: sizer(func(x *rd) (int64, error) {
		if l := pdfLinearizedLen(x); l > 0 {
			return l, nil
		}
		pos, last := int64(5), int64(-1)
		end := func() (int64, error) {
			if !pdfXrefOK(x, last) {
				return 0, errTrunc
			}
			return last, nil
		}
		for {
			stop := x.limit
			if last >= 0 {
				stop = min(x.limit, last+8*mib)
			}
			i, pat, err := x.find(pos, stop, []byte("%%EOF"), []byte("%PDF-"))
			if err != nil {
				if last >= 0 && (errors.Is(err, errNotFound) || errors.Is(err, errTrunc)) {
					return end()
				}
				return 0, err
			}
			if string(pat) == "%PDF-" {
				if last < 0 {
					return 0, errBad
				}
				return end()
			}
			e := i + 5
			if b, err := x.bytes(e, 2); err == nil && string(b) == "\r\n" {
				e += 2
			} else if b, err := x.bytes(e, 1); err == nil && (b[0] == '\n' || b[0] == '\r') {
				e++
			}
			last, pos = e, i+5
		}
	})}

// pdfLinearizedLen is the length a linearized PDF states in its first object (/L), when the
// file really ends with %%EOF there; 0 otherwise.
func pdfLinearizedLen(x *rd) int64 {
	h, err := x.bytes(0, int(min(1024, x.limit)))
	if err != nil {
		return 0
	}
	i := strings.Index(string(h), "/Linearized")
	if i < 0 {
		return 0
	}
	j := strings.Index(string(h[i:]), "/L ")
	if j < 0 {
		return 0
	}
	var l int64
	for _, c := range h[i+j+3:] {
		if c < '0' || c > '9' {
			break
		}
		l = l*10 + int64(c-'0')
	}
	if l < 64 || l > x.limit {
		return 0
	}
	t, err := x.bytes(l-64, 64)
	if err != nil || !strings.Contains(string(t), "%%EOF") {
		return 0
	}
	return l
}

// pdfXrefOK checks the startxref before a PDF's end points at an xref table or an object inside
// the file. Without a startxref there is nothing to check.
func pdfXrefOK(x *rd, end int64) bool {
	from := max(0, end-1100)
	b, err := x.bytes(from, int(end-from))
	if err != nil {
		return true
	}
	k := strings.LastIndex(string(b), "startxref")
	if k < 0 {
		return true
	}
	var n int64
	digits := 0
	for _, c := range b[k+len("startxref"):] {
		if c >= '0' && c <= '9' {
			n, digits = n*10+int64(c-'0'), digits+1
		} else if digits > 0 {
			break
		}
	}
	if digits == 0 || n >= end {
		return false
	}
	t, err := x.bytes(n, int(min(32, end-n)))
	if err != nil {
		return false
	}
	s := string(t)
	if strings.HasPrefix(s, "xref") {
		return true
	}
	var a, g int // an xref stream: "12 0 obj"
	var w string
	c, _ := fmt.Sscanf(s, "%d %d %s", &a, &g, &w)
	return c == 3 && strings.HasPrefix(w, "obj")
}

// apkSigBlock returns the size (after its first size field) of an APK Signing Block at pos.
func apkSigBlock(x *rd, pos int64) (int64, bool) {
	h, err := x.bytes(pos, 8)
	if err != nil {
		return 0, false
	}
	n := le32(h) | le32(h[4:])<<32
	if n < 24 || n > 64*mib {
		return 0, false
	}
	t, err := x.bytes(pos+8+n-16, 16)
	if err != nil || string(t) != "APK Sig Block 42" {
		return 0, false
	}
	return n, true
}

var zipMethods = map[int64]bool{0: true, 1: true, 6: true, 8: true, 9: true, 12: true, 14: true, 93: true, 95: true, 98: true, 99: true}

// zipWalk walks local headers, the central directory and the end record; entry receives the
// first entries' names (with their data offset, method and stored size).
func zipWalk(x *rd, entry func(name string, data, method, csize int64)) (int64, error) {
	pos, files := int64(0), 0
	for {
		if err := x.step(); err != nil {
			return 0, err
		}
		sig, err := x.bytes(pos, 4)
		if err != nil {
			return 0, err
		}
		if sig[0] != 'P' || sig[1] != 'K' {
			// an Android APK Signing Block sits between the last file and the central directory:
			// u64 size, entries, u64 size again, then the magic "APK Sig Block 42"
			if files > 0 {
				if n, ok := apkSigBlock(x, pos); ok {
					pos += 8 + n
					continue
				}
			}
			return 0, errBad
		}
		switch string(sig[2:]) {
		case "\x03\x04":
			h, err := x.bytes(pos, 30)
			if err != nil {
				return 0, err
			}
			flags, method, csize := le16(h[6:]), le16(h[8:]), le32(h[18:])
			nlen, xlen := le16(h[26:]), le16(h[28:])
			if !zipMethods[method] || nlen == 0 || nlen > 1024 || le16(h[4:]) > 63 {
				return 0, errBad
			}
			data := pos + 30 + nlen + xlen
			if entry != nil && files < 16 {
				if name, err := x.bytes(pos+30, int(nlen)); err == nil {
					entry(string(name), data, method, csize)
				}
			}
			files++
			if flags&8 != 0 && (csize == 0 || csize == 0xFFFFFFFF) || csize == 0xFFFFFFFF {
				q, pat, err := x.find(data, x.limit, []byte("PK\x07\x08"), []byte("PK\x03\x04"), []byte("PK\x01\x02"))
				if err != nil {
					return 0, err
				}
				pos = q
				if string(pat) == "PK\x07\x08" {
					pos = q + 16
					if nx, err := x.bytes(q+16, 2); err == nil && string(nx) != "PK" {
						pos = q + 24 // ZIP64 descriptor
					}
				}
				continue
			}
			pos = data + csize
			if flags&8 != 0 {
				if d, err := x.bytes(pos, 4); err == nil && string(d) == "PK\x07\x08" {
					pos += 16
				} else {
					pos += 12
				}
			}
		case "\x01\x02":
			h, err := x.bytes(pos, 46)
			if err != nil {
				return 0, err
			}
			pos += 46 + le16(h[28:]) + le16(h[30:]) + le16(h[32:])
		case "\x05\x06":
			h, err := x.bytes(pos, 22)
			if err != nil {
				return 0, err
			}
			end := pos + 22 + le16(h[20:])
			if files == 0 {
				return 0, errBad
			}
			if _, err := x.bytes(end-1, 1); err != nil {
				return 0, err
			}
			return end, nil
		case "\x06\x06":
			h, err := x.bytes(pos, 12)
			if err != nil {
				return 0, err
			}
			pos += 12 + int64(le64(h[4:])&(1<<40-1))
		case "\x06\x07":
			pos += 20
		case "\x05\x05":
			h, err := x.bytes(pos, 6)
			if err != nil {
				return 0, err
			}
			pos += 6 + le16(h[4:])
		default:
			return 0, errBad
		}
	}
}

// ZIP family (DOCX/XLSX/PPTX, ODF, EPUB, APK, ZIP): local headers → central directory → end record.
var zipFormat = Format{Name: "zip", Ext: "zip", Cat: profile.Archives, MaxSize: 8 * gib,
	Magics: []Magic{m(0, "PK\x03\x04")},
	Size:   sizer(func(x *rd) (int64, error) { return zipWalk(x, nil) }),
	Sub: func(r io.ReaderAt, off, n int64) string {
		x := &rd{r: r, base: off, limit: n}
		ext := ""
		zipWalk(x, func(name string, data, method, csize int64) {
			switch {
			case ext != "":
			case name == "mimetype" && method == 0 && csize > 0 && csize < 128:
				b, _ := x.bytes(data, int(csize))
				switch mt := string(b); {
				case mt == "application/epub+zip":
					ext = "epub"
				case strings.HasSuffix(mt, "opendocument.text"):
					ext = "odt"
				case strings.HasSuffix(mt, "opendocument.spreadsheet"):
					ext = "ods"
				case strings.HasSuffix(mt, "opendocument.presentation"):
					ext = "odp"
				}
			case strings.HasPrefix(name, "word/"):
				ext = "docx"
			case strings.HasPrefix(name, "xl/"):
				ext = "xlsx"
			case strings.HasPrefix(name, "ppt/"):
				ext = "pptx"
			case name == "AndroidManifest.xml" || name == "classes.dex":
				ext = "apk"
			}
		})
		return ext
	}}

// 7z: the start header gives the next header's offset and size.
var sevenZipFormat = Format{HeaderSized: true, Name: "7z", Ext: "7z", Cat: profile.Archives, MaxSize: 64 * gib,
	Magics: []Magic{m(0, "7z\xBC\xAF\x27\x1C")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 32)
		if err != nil {
			return 0, err
		}
		off, n := le64(h[12:]), le64(h[20:])
		if h[6] != 0 || n == 0 || n > 1<<32 || off > 1<<40 {
			return 0, errBad
		}
		end := 32 + int64(off+n)
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	})}

func rar5Vint(b []byte) (int64, int) {
	var v int64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= int64(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// RAR 4 and 5: block walk to the end-of-archive block.
var rarFormat = Format{Name: "rar", Ext: "rar", Cat: profile.Archives, MaxSize: 64 * gib,
	Magics: []Magic{m(0, "Rar!\x1A\x07")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 8)
		if err != nil {
			return 0, err
		}
		switch {
		case h[6] == 0:
			pos := int64(7)
			for {
				if err := x.step(); err != nil {
					return 0, err
				}
				b, err := x.bytes(pos, 7)
				if err != nil {
					return 0, err
				}
				typ, flags, size := b[2], le16(b[3:]), le16(b[5:])
				if size < 7 || typ < 0x72 || typ > 0x7B {
					return 0, errBad
				}
				if flags&0x8000 != 0 {
					a, err := x.bytes(pos+7, 4)
					if err != nil {
						return 0, err
					}
					size += le32(a)
				}
				pos += size
				if typ == 0x7B {
					if _, err := x.bytes(pos-1, 1); err != nil {
						return 0, err
					}
					return pos, nil
				}
			}
		case h[6] == 1 && h[7] == 0:
			pos := int64(8)
			for {
				if err := x.step(); err != nil {
					return 0, err
				}
				b, err := x.bytes(pos+4, 3)
				if err != nil {
					return 0, err
				}
				hs, hn := rar5Vint(b)
				if hn == 0 || hs < 2 || hs > 2*mib {
					return 0, errBad
				}
				hd, err := x.bytes(pos+4+int64(hn), int(hs))
				if err != nil {
					return 0, err
				}
				typ, n1 := rar5Vint(hd)
				flags, n2 := rar5Vint(hd[n1:])
				if n1 == 0 || n2 == 0 || typ < 1 || typ > 5 {
					return 0, errBad
				}
				rest := hd[n1+n2:]
				if flags&1 != 0 {
					_, k := rar5Vint(rest)
					if k == 0 {
						return 0, errBad
					}
					rest = rest[k:]
				}
				var data int64
				if flags&2 != 0 {
					d, k := rar5Vint(rest)
					if k == 0 {
						return 0, errBad
					}
					data = d
				}
				pos += 4 + int64(hn) + hs + data
				if typ == 5 {
					if _, err := x.bytes(pos-1, 1); err != nil {
						return 0, err
					}
					return pos, nil
				}
			}
		}
		return 0, errBad
	})}

// byteReader hands flate one byte at a time (so it never reads past the stream's end) from a
// buffered window of the candidate.
type byteReader struct {
	x        *rd
	pos      int64 // next byte to hand out
	buf      []byte
	bufStart int64
}

func (b *byteReader) ReadByte() (byte, error) {
	if b.pos < b.bufStart || b.pos >= b.bufStart+int64(len(b.buf)) {
		n := min(64*kib, b.x.limit-b.pos)
		if n <= 0 {
			b.x.seen = b.x.limit
			return 0, io.EOF
		}
		c, err := b.x.bytes(b.pos, int(n))
		if err != nil {
			return 0, err
		}
		b.buf, b.bufStart = c, b.pos
	}
	c := b.buf[b.pos-b.bufStart]
	b.pos++
	return c, nil
}

func (b *byteReader) Read(p []byte) (int, error) {
	for i := range p {
		c, err := b.ReadByte()
		if err != nil {
			return i, err
		}
		p[i] = c
	}
	return len(p), nil
}

// GZIP: header, the deflate stream to its end, the trailer (CRC and size checked); members repeat.
var gzipFormat = Format{Name: "gzip", Ext: "gz", Cat: profile.Archives, MaxSize: 8 * gib,
	Magics: []Magic{{0, []byte{0x1F, 0x8B, 0x08}}}, Size: sizer(func(x *rd) (int64, error) {
		pos := int64(0)
		for member := 0; ; member++ {
			h, err := x.bytes(pos, 10)
			if err != nil {
				if member > 0 {
					return pos, nil
				}
				return 0, err
			}
			if h[0] != 0x1F || h[1] != 0x8B || h[2] != 8 || h[3]&0xE0 != 0 {
				if member > 0 {
					return pos, nil
				}
				return 0, errBad
			}
			flg := h[3]
			p := pos + 10
			if flg&4 != 0 {
				l, err := x.bytes(p, 2)
				if err != nil {
					return 0, err
				}
				p += 2 + le16(l)
			}
			for _, bit := range []byte{8, 16} {
				if flg&bit != 0 {
					q, _, err := x.find(p, p+64*kib, []byte{0})
					if err != nil {
						return 0, errBad
					}
					p = q + 1
				}
			}
			if flg&2 != 0 {
				p += 2
			}
			br := &byteReader{x: x, pos: p}
			fr := flate.NewReader(br)
			sum := crc32.NewIEEE()
			n, err := io.Copy(sum, io.LimitReader(fr, 4*gib))
			if err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, errTrunc) {
					x.seen = max(x.seen, br.pos)
					return 0, errTrunc
				}
				return 0, errBad
			}
			if n == 4*gib {
				return 0, errSteps
			}
			t, err := x.bytes(br.pos, 8)
			if err != nil {
				return 0, err
			}
			if uint32(le32(t)) != sum.Sum32() || uint32(le32(t[4:])) != uint32(n) {
				return 0, errBad
			}
			pos = br.pos + 8
		}
	})}

// SQLite: page size × page count from the header (when the count is valid).
var sqliteFormat = Format{HeaderSized: true, Name: "sqlite", Ext: "sqlite", Cat: profile.Other, MaxSize: 16 * gib,
	Magics: []Magic{m(0, "SQLite format 3\x00")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 100)
		if err != nil {
			return 0, err
		}
		ps := be16(h[16:])
		if ps == 1 {
			ps = 65536
		}
		if ps < 512 || ps&(ps-1) != 0 || h[21] != 64 || h[22] != 32 || h[23] != 32 {
			return 0, errBad
		}
		count := be32(h[28:])
		if count == 0 || be32(h[92:]) != be32(h[24:]) {
			x.seen = ps
			return 0, errTrunc // pre-3.7 header: the size isn't recorded
		}
		end := ps * count
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	})}

// OLE2 (DOC, XLS, PPT, MSG): the highest sector the FAT references.
var ole2Format = Format{HeaderSized: true, Name: "ole2", Ext: "ole", Cat: profile.Other, MaxSize: 4 * gib,
	Magics: []Magic{{0, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}}},
	Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 512)
		if err != nil {
			return 0, err
		}
		shift := le16(h[30:])
		if shift != 9 && shift != 12 || h[28] != 0xFE || h[29] != 0xFF {
			return 0, errBad
		}
		ss := int64(1) << shift
		nfat := le32(h[44:])
		if nfat == 0 || nfat > x.limit/ss {
			return 0, errBad
		}
		var fats []int64
		for i := 0; i < 109 && int64(len(fats)) < nfat; i++ {
			fats = append(fats, le32(h[76+i*4:]))
		}
		for d, n := le32(h[68:]), 0; int64(len(fats)) < nfat; n++ {
			if err := x.step(); err != nil || d >= 0xFFFFFFFA || n > 1<<16 {
				return 0, errBad
			}
			b, err := x.bytes((d+1)*ss, int(ss))
			if err != nil {
				return 0, err
			}
			for i := int64(0); i < ss/4-1 && int64(len(fats)) < nfat; i++ {
				fats = append(fats, le32(b[i*4:]))
			}
			d = le32(b[ss-4:])
		}
		maxSec := int64(-1)
		for k, s := range fats {
			if err := x.step(); err != nil {
				return 0, err
			}
			if s >= 0xFFFFFFFA {
				return 0, errBad
			}
			b, err := x.bytes((s+1)*ss, int(ss))
			if err != nil {
				return 0, err
			}
			for j := int64(0); j < ss/4; j++ {
				if le32(b[j*4:]) != 0xFFFFFFFF {
					maxSec = max(maxSec, int64(k)*(ss/4)+j)
				}
			}
		}
		end := (maxSec + 2) * ss
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		x := &rd{r: r, base: off, limit: n}
		h, err := x.bytes(0, 512)
		if err != nil {
			return ""
		}
		ss := int64(1) << le16(h[30:])
		d, err := x.bytes((le32(h[48:])+1)*ss, int(ss))
		if err != nil {
			return ""
		}
		for e := int64(0); e+128 <= ss; e += 128 {
			l := le16(d[e+64:])
			if l < 2 || l > 64 {
				continue
			}
			u := make([]uint16, (l-2)/2)
			for i := range u {
				u[i] = uint16(le16(d[e+int64(i)*2:]))
			}
			switch name := string(utf16.Decode(u)); {
			case name == "WordDocument":
				return "doc"
			case name == "Workbook" || name == "Book":
				return "xls"
			case name == "PowerPoint Document":
				return "ppt"
			case strings.HasPrefix(name, "__substg1.0_"):
				return "msg"
			}
		}
		return ""
	}}

// RTF: brace depth back to zero.
var rtfFormat = Format{Name: "rtf", Ext: "rtf", Cat: profile.Documents, MaxSize: 256 * mib,
	Magics: []Magic{m(0, `{\rtf`)}, Size: sizer(func(x *rd) (int64, error) {
		pos, depth, esc := int64(0), 0, false
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			n := min(64*kib, x.limit-pos)
			if n <= 0 {
				x.seen = x.limit
				return 0, errTrunc
			}
			b, err := x.bytes(pos, int(n))
			if err != nil {
				return 0, err
			}
			for i, c := range b {
				switch {
				case c == 0:
					return 0, errBad
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '{':
					depth++
				case c == '}':
					depth--
					if depth == 0 {
						return pos + int64(i) + 1, nil
					}
				}
			}
			pos += n
		}
	})}
