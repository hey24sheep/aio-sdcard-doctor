package carve

import (
	"bytes"
	"io"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// RIFF (WebP, WAV, AVI + AVIX extensions): chunk sizes.
var riffFormat = Format{HeaderSized: true, Name: "riff", Ext: "wav", Cat: profile.Music, MaxSize: 4 * gib,
	Magics: []Magic{m(0, "RIFF")},
	Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 12)
		if err != nil {
			return 0, err
		}
		form := string(h[8:12])
		if riffExt(form) == "" {
			return 0, errBad
		}
		n := le32(h[4:])
		if n < 4 {
			return 0, errBad
		}
		pos := 8 + n + n&1
		for form == "AVI " { // AVIX extension chunks follow a large AVI
			if err := x.step(); err != nil {
				return 0, err
			}
			nx, err := x.bytes(pos, 12)
			if err != nil || string(nx[:4]) != "RIFF" || string(nx[8:12]) != "AVIX" {
				break
			}
			n := le32(nx[4:])
			pos += 8 + n + n&1
		}
		if _, err := x.bytes(pos-1, 1); err != nil {
			return 0, err
		}
		return pos, nil
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		b := make([]byte, 4)
		if _, err := r.ReadAt(b, off+8); err != nil {
			return ""
		}
		return riffExt(string(b))
	}}

func riffExt(form string) string {
	return map[string]string{"WEBP": "webp", "WAVE": "wav", "AVI ": "avi"}[form]
}

// AIFF: FORM chunk size.
var aiffFormat = Format{HeaderSized: true, Name: "aiff", Ext: "aiff", Cat: profile.Music, MaxSize: 4 * gib,
	Magics: []Magic{m(0, "FORM")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 12)
		if err != nil {
			return 0, err
		}
		if f := string(h[8:12]); f != "AIFF" && f != "AIFC" {
			return 0, errBad
		}
		n := be32(h[4:])
		end := 8 + n + n&1
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	})}

var bmffTop = map[string]bool{"ftyp": true, "moov": true, "mdat": true, "free": true, "skip": true, "wide": true,
	"uuid": true, "meta": true, "pdin": true, "moof": true, "mfra": true, "styp": true, "sidx": true, "ssix": true,
	"prft": true, "emsg": true, "udta": true, "pnot": true, "junk": true}

// ISO-BMFF (MP4, MOV, 3GP, M4V, M4A, HEIC, AVIF, CR3, INSV): top-level box walk.
var bmffFormat = Format{HeaderSized: true, Name: "isobmff", Ext: "mp4", Cat: profile.Videos, MaxSize: 64 * gib,
	Magics: []Magic{m(4, "ftyp")},
	Size: sizer(func(x *rd) (int64, error) {
		var moov, mdat, meta bool
		ok := func() bool { return moov && mdat || meta }
		pos := int64(0)
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			if pos == x.limit && ok() {
				return pos, nil
			}
			h, err := x.bytes(pos, 8)
			if err != nil {
				return 0, err
			}
			typ := string(h[4:8])
			if !bmffTop[typ] || pos == 0 && typ != "ftyp" {
				if pos == 0 {
					return 0, errBad
				}
				if ok() {
					return pos, nil
				}
				x.seen = pos
				return 0, errTrunc
			}
			size, hdr := be32(h), int64(8)
			switch size {
			case 1:
				b, err := x.bytes(pos+8, 8)
				if err != nil {
					return 0, err
				}
				size, hdr = int64(be64(b)&(1<<62-1)), 16
			case 0: // runs to the end of the file: its end can't be known
				x.seen = x.limit
				return 0, errTrunc
			}
			if size < hdr {
				return 0, errBad
			}
			switch typ {
			case "moov":
				moov = true
			case "mdat":
				mdat = true
			case "meta":
				meta = true
			}
			pos += size
			if pos > x.limit {
				x.seen = x.limit
				return 0, errTrunc
			}
		}
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		b := make([]byte, 4)
		if _, err := r.ReadAt(b, off+8); err != nil {
			return ""
		}
		switch br := string(b); {
		case br == "qt  ":
			return "mov"
		case br == "heic" || br == "heix" || br == "mif1" || br == "msf1" || br == "heim" || br == "heis" || br == "hevc":
			return "heic"
		case br == "avif" || br == "avis":
			return "avif"
		case br == "crx ":
			return "cr3"
		case br[:3] == "3gp" || br[:3] == "3g2":
			return "3gp"
		case br == "M4V " || br == "M4VH":
			return "m4v"
		case br == "M4A " || br == "M4B ":
			return "m4a"
		}
		return "mp4"
	}}

// ebmlVint reads an EBML variable-length size; unknown=true for the all-ones "unknown size".
func ebmlVint(x *rd, pos int64) (v int64, n int, unknown bool, err error) {
	b, err := x.bytes(pos, 1)
	if err != nil {
		return 0, 0, false, err
	}
	n = 1
	for mask := byte(0x80); n <= 8 && b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 {
		return 0, 0, false, errBad
	}
	all, err := x.bytes(pos, n)
	if err != nil {
		return 0, 0, false, err
	}
	v = int64(all[0] & (0xFF >> n))
	ones := v == int64(0xFF>>n)
	for _, c := range all[1:] {
		v = v<<8 | int64(c)
		ones = ones && c == 0xFF
	}
	return v, n, ones, nil
}

func ebmlID(x *rd, pos int64) (uint32, int, error) {
	b, err := x.bytes(pos, 1)
	if err != nil {
		return 0, 0, err
	}
	n := 1
	for mask := byte(0x80); n <= 4 && b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 4 {
		return 0, 0, errBad
	}
	all, err := x.bytes(pos, n)
	if err != nil {
		return 0, 0, err
	}
	var id uint32
	for _, c := range all {
		id = id<<8 | uint32(c)
	}
	return id, n, nil
}

var segmentChildren = map[uint32]bool{0x114D9B74: true, 0x1549A966: true, 0x1654AE6B: true, 0x1F43B675: true,
	0x1C53BB6B: true, 0x1941A469: true, 0x1043A770: true, 0x1254C367: true, 0xEC: true, 0xBF: true}

// Matroska / WebM: EBML header, then the Segment size (or a walk of its children when unknown).
var ebmlFormat = Format{HeaderSized: true, Name: "ebml", Ext: "mkv", Cat: profile.Videos, MaxSize: 64 * gib,
	Magics: []Magic{{0, []byte{0x1A, 0x45, 0xDF, 0xA3}}},
	Size: sizer(func(x *rd) (int64, error) {
		hs, n, unk, err := ebmlVint(x, 4)
		if err != nil {
			return 0, err
		}
		if unk || hs > 4096 {
			return 0, errBad
		}
		pos := 4 + int64(n) + hs
		id, idn, err := ebmlID(x, pos)
		if err != nil {
			return 0, err
		}
		if id != 0x18538067 {
			return 0, errBad
		}
		ss, sn, unk, err := ebmlVint(x, pos+int64(idn))
		if err != nil {
			return 0, err
		}
		pos += int64(idn + sn)
		if !unk {
			end := pos + ss
			if _, err := x.bytes(end-1, 1); err != nil {
				return 0, err
			}
			return end, nil
		}
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			id, idn, err := ebmlID(x, pos)
			if err != nil {
				return 0, err
			}
			if !segmentChildren[id] {
				return pos, nil
			}
			cs, cn, unk, err := ebmlVint(x, pos+int64(idn))
			if err != nil {
				return 0, err
			}
			if unk {
				x.seen = x.limit
				return 0, errTrunc
			}
			pos += int64(idn+cn) + cs
		}
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		b := make([]byte, min(n, 64))
		r.ReadAt(b, off)
		if bytes.Contains(b, []byte{0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}) {
			return "webm"
		}
		return "mkv"
	}}

// MPEG-TS / M2TS: a run of 188- or 192-byte packets with sync bytes.
var tsFormat = Format{Name: "mpegts", Ext: "ts", Cat: profile.Videos, MaxSize: 64 * gib,
	Magics: []Magic{{0, []byte{0x47}}, {4, []byte{0x47}}},
	Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 4+192*2+1)
		if err != nil {
			return 0, err
		}
		stride, sync := int64(0), int64(0)
		switch {
		case h[0] == 0x47 && h[188] == 0x47 && h[376] == 0x47:
			stride = 188
		case h[4] == 0x47 && h[196] == 0x47 && h[388] == 0x47:
			stride, sync = 192, 4
		default:
			return 0, errBad
		}
		const batch = 256
		packets := int64(0)
		pos := int64(0)
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			n := min(batch, (x.limit-pos)/stride)
			if n == 0 { // less than one packet left: its sync byte still tells whether the run ends here
				if x.limit-pos > sync && packets >= 16 {
					if b, err := x.bytes(pos+sync, 1); err == nil && b[0] != 0x47 {
						return pos, nil
					}
				}
				x.seen = x.limit
				return 0, errTrunc
			}
			b, err := x.bytes(pos, int(n*stride))
			if err != nil {
				return 0, err
			}
			for i := int64(0); i < n; i++ {
				if b[i*stride+sync] != 0x47 {
					if packets < 16 {
						return 0, errBad
					}
					return pos + i*stride, nil
				}
				packets++
			}
			pos += n * stride
		}
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		b := make([]byte, 1)
		if r.ReadAt(b, off); b[0] != 0x47 {
			return "m2ts"
		}
		return "ts"
	}}

// MPEG-PS (MPG, VOB, MOD, TOD): pack headers and PES packets to the end code 000001B9.
var psFormat = Format{Name: "mpegps", Ext: "mpg", Cat: profile.Videos, MaxSize: 64 * gib,
	Magics: []Magic{{0, []byte{0, 0, 1, 0xBA}}}, Size: sizer(func(x *rd) (int64, error) {
		pos, packs := int64(0), 0
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 4)
			if err != nil {
				return 0, err
			}
			if b[0] != 0 || b[1] != 0 || b[2] != 1 {
				if packs >= 2 {
					return pos, nil
				}
				return 0, errBad
			}
			switch c := b[3]; {
			case c == 0xB9:
				return pos + 4, nil
			case c == 0xBA:
				h, err := x.bytes(pos, 14)
				if err != nil {
					return 0, err
				}
				switch {
				case h[4]&0xC0 == 0x40:
					pos += 14 + int64(h[13]&7)
				case h[4]&0xF0 == 0x20:
					pos += 12
				default:
					return 0, errBad
				}
				packs++
			case c >= 0xBB:
				l, err := x.bytes(pos+4, 2)
				if err != nil {
					return 0, err
				}
				pos += 6 + be16(l)
			default:
				if packs >= 2 {
					return pos, nil
				}
				return 0, errBad
			}
		}
	})}

// FLV: tag sizes, each confirmed by the following PreviousTagSize.
var flvFormat = Format{Name: "flv", Ext: "flv", Cat: profile.Videos, MaxSize: 16 * gib,
	Magics: []Magic{m(0, "FLV\x01")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 9)
		if err != nil {
			return 0, err
		}
		pos := be32(h[5:]) + 4
		if pos != 13 || h[4]&^5 != 0 {
			return 0, errBad
		}
		tags := 0
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			t, err := x.bytes(pos, 11)
			if err != nil {
				return 0, err
			}
			if t[0] != 8 && t[0] != 9 && t[0] != 18 {
				break
			}
			n := 11 + (be32(t) & 0xFFFFFF)
			p, err := x.bytes(pos+n, 4)
			if err != nil {
				return 0, err
			}
			if be32(p) != n {
				break
			}
			pos += n + 4
			tags++
		}
		if tags == 0 {
			return 0, errBad
		}
		return pos, nil
	})}

var asfObjects = [][]byte{
	{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}, // header
	{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}, // data
	{0x90, 0x08, 0x00, 0x33, 0xB1, 0xE5, 0xCF, 0x11, 0x89, 0xF4, 0x00, 0xA0, 0xC9, 0x03, 0x49, 0xCB}, // simple index
	{0xD3, 0x29, 0xE2, 0xD6, 0xDA, 0x35, 0xD1, 0x11, 0x90, 0x34, 0x00, 0xA0, 0xC9, 0x03, 0x49, 0xBE}, // index
}

// WMV/WMA (ASF): top-level object sizes.
var asfFormat = Format{HeaderSized: true, Name: "asf", Ext: "wmv", Cat: profile.Videos, MaxSize: 16 * gib,
	Magics: []Magic{{0, asfObjects[0]}}, Size: sizer(func(x *rd) (int64, error) {
		pos, data := int64(0), false
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			if pos == x.limit && data {
				return pos, nil
			}
			h, err := x.bytes(pos, 24)
			if err != nil {
				return 0, err
			}
			known := -1
			for i, g := range asfObjects {
				if bytes.Equal(h[:16], g) {
					known = i
				}
			}
			if known < 0 {
				if data {
					return pos, nil
				}
				return 0, errBad
			}
			size := int64(le64(h[16:]) & (1<<62 - 1))
			if size < 24 {
				return 0, errBad
			}
			data = data || known == 1
			pos += size
			if pos > x.limit {
				x.seen = x.limit
				return 0, errTrunc
			}
		}
	})}
