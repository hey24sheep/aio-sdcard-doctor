package carve

import (
	"bytes"
	"io"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// JPEG: segments to SOS, entropy data to FFD9 (APP segments, incl. EXIF thumbnails, skipped by length).
var jpegFormat = Format{Name: "jpeg", Ext: "jpg", Cat: profile.Photos, MaxSize: 256 * mib,
	Magics: []Magic{{0, []byte{0xFF, 0xD8, 0xFF}}}, Size: sizer(jpegWalk)}

func jpegWalk(x *rd) (int64, error) {
	pos := int64(2)
	first := true
	for {
		if err := x.step(); err != nil {
			return 0, err
		}
		b, err := x.bytes(pos, 2)
		if err != nil {
			return 0, err
		}
		if b[0] != 0xFF {
			return 0, errBad
		}
		mk := b[1]
		switch {
		case mk == 0xFF: // fill byte
			pos++
			continue
		case mk == 0xD9:
			if first {
				return 0, errBad
			}
			return pos + 2, nil
		case mk >= 0xD0 && mk <= 0xD7 || mk == 0x01:
			pos += 2
			continue
		case mk < 0xC0 || mk == 0xD8:
			return 0, errBad
		}
		if first && !(mk >= 0xE0 && mk <= 0xEF || mk == 0xDB || mk == 0xFE || mk == 0xC4 || mk >= 0xC0 && mk <= 0xCF) {
			return 0, errBad
		}
		first = false
		l, err := x.bytes(pos+2, 2)
		if err != nil {
			return 0, err
		}
		n := be16(l)
		if n < 2 {
			return 0, errBad
		}
		pos += 2 + n
		if mk != 0xDA {
			continue
		}
		// entropy-coded data: up to the next marker that isn't a stuffed 00 or a restart marker
		for found := false; !found; {
			if err := x.step(); err != nil {
				return 0, err
			}
			n := min(64*kib, x.limit-pos)
			if n < 2 {
				_, err := x.bytes(pos, 2)
				return 0, err
			}
			c, err := x.bytes(pos, int(n))
			if err != nil {
				return 0, err
			}
			for i := 0; i+1 < len(c); i++ {
				if c[i] != 0xFF {
					continue
				}
				if d := c[i+1]; d == 0 || d >= 0xD0 && d <= 0xD7 || d == 0xFF {
					continue
				}
				pos += int64(i)
				found = true
				break
			}
			if !found {
				pos += n - 1
			}
		}
	}
}

// PNG: chunks to IEND.
var pngFormat = Format{Name: "png", Ext: "png", Cat: profile.Photos, MaxSize: 256 * mib,
	Magics: []Magic{m(0, "\x89PNG\r\n\x1a\n")}, Size: sizer(func(x *rd) (int64, error) {
		pos := int64(8)
		for i := 0; ; i++ {
			if err := x.step(); err != nil {
				return 0, err
			}
			h, err := x.bytes(pos, 8)
			if err != nil {
				return 0, err
			}
			n := be32(h)
			typ := h[4:8]
			for _, c := range typ {
				if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
					return 0, errBad
				}
			}
			if n > 1<<31-1 || i == 0 && string(typ) != "IHDR" {
				return 0, errBad
			}
			pos += 12 + n
			if string(typ) == "IEND" {
				if _, err := x.bytes(pos-1, 1); err != nil {
					return 0, err
				}
				return pos, nil
			}
		}
	})}

// GIF: header, colour tables, blocks to the trailer.
var gifFormat = Format{Name: "gif", Ext: "gif", Cat: profile.Photos, MaxSize: 64 * mib,
	Magics: []Magic{m(0, "GIF87a"), m(0, "GIF89a")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 13)
		if err != nil {
			return 0, err
		}
		pos := int64(13)
		if h[10]&0x80 != 0 {
			pos += 3 << (h[10]&7 + 1)
		}
		subBlocks := func() error {
			for {
				if err := x.step(); err != nil {
					return err
				}
				b, err := x.bytes(pos, 1)
				if err != nil {
					return err
				}
				pos += 1 + int64(b[0])
				if b[0] == 0 {
					return nil
				}
			}
		}
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 1)
			if err != nil {
				return 0, err
			}
			switch b[0] {
			case 0x3B:
				return pos + 1, nil
			case 0x21:
				pos += 2
				if err := subBlocks(); err != nil {
					return 0, err
				}
			case 0x2C:
				d, err := x.bytes(pos, 10)
				if err != nil {
					return 0, err
				}
				pos += 10
				if d[9]&0x80 != 0 {
					pos += 3 << (d[9]&7 + 1)
				}
				pos++ // LZW minimum code size
				if err := subBlocks(); err != nil {
					return 0, err
				}
			default:
				return 0, errBad
			}
		}
	})}

// BMP: the size in its header.
var bmpFormat = Format{HeaderSized: true, Name: "bmp", Ext: "bmp", Cat: profile.Photos, MaxSize: 256 * mib,
	Magics: []Magic{m(0, "BM")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 30)
		if err != nil {
			return 0, err
		}
		size, data, dib := le32(h[2:]), le32(h[10:]), le32(h[14:])
		switch dib {
		case 12, 40, 52, 56, 64, 108, 124:
		default:
			return 0, errBad
		}
		planes := h[26:]
		if dib == 12 {
			planes = h[22:]
		}
		if le32(h[6:]) != 0 || data < 14+dib || data >= size || le16(planes) != 1 {
			return 0, errBad
		}
		if _, err := x.bytes(size-1, 1); err != nil {
			return 0, err
		}
		return size, nil
	})}

// TIFF and TIFF-based RAW: IFD walk → furthest strip/tile/JPEG/SubIFD/value.
var tiffFormat = Format{HeaderSized: true, Name: "tiff", Ext: "tif", Cat: profile.Photos, MaxSize: 512 * mib,
	Magics: []Magic{m(0, "II*\x00"), m(0, "MM\x00*"), m(0, "IIRO"), m(0, "IIRS"), m(0, "MMOR"), m(0, "IIU\x00")},
	Size: sizer(func(x *rd) (int64, error) {
		end, _, err := tiffWalk(x)
		if err != nil {
			return 0, err
		}
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		x := &rd{r: r, base: off, limit: n}
		h, err := x.bytes(0, 10)
		if err != nil {
			return ""
		}
		switch {
		case string(h[:4]) == "IIRO" || string(h[:4]) == "IIRS" || string(h[:4]) == "MMOR":
			return "orf"
		case string(h[:4]) == "IIU\x00":
			return "rw2"
		case string(h[8:10]) == "CR":
			return "cr2"
		}
		_, info, _ := tiffWalk(x)
		if info.dng {
			return "dng"
		}
		mk := strings.ToUpper(info.make)
		for _, p := range []struct{ prefix, ext string }{{"NIKON", "nef"}, {"SONY", "arw"}, {"PENTAX", "pef"},
			{"RICOH", "pef"}, {"SAMSUNG", "srw"}, {"OLYMPUS", "orf"}, {"PANASONIC", "rw2"}, {"CANON", "cr2"}} {
			if strings.HasPrefix(mk, p.prefix) {
				return p.ext
			}
		}
		return ""
	}}

type tiffInfo struct {
	make string
	dng  bool
}

var tiffTypeSize = [19]int64{0, 1, 1, 2, 4, 8, 1, 1, 2, 4, 8, 4, 8, 4, 0, 0, 8, 8, 8}

func tiffWalk(x *rd) (int64, tiffInfo, error) {
	var info tiffInfo
	h, err := x.bytes(0, 8)
	if err != nil {
		return 0, info, err
	}
	u16, u32 := le16, le32
	if h[0] == 'M' {
		u16, u32 = be16, be32
	}
	end := int64(8)
	queue := []int64{u32(h[4:])}
	seen := map[int64]bool{}
	// values reads count values of an entry (inline when they fit in 4 bytes)
	values := func(e []byte, typ, count int64) ([]int64, error) {
		if count > 1<<16 {
			return nil, errBad
		}
		ts := tiffTypeSize[typ]
		raw := e[8:12]
		if ts*count > 4 {
			b, err := x.bytes(u32(e[8:]), int(ts*count))
			if err != nil {
				return nil, err
			}
			raw = b
		}
		out := make([]int64, count)
		for i := range out {
			if ts == 2 {
				out[i] = u16(raw[i*2:])
			} else {
				out[i] = u32(raw[i*4:])
			}
		}
		return out, nil
	}
	for ifd := 0; len(queue) > 0; ifd++ {
		o := queue[0]
		queue = queue[1:]
		if o == 0 || seen[o] {
			continue
		}
		if err := x.step(); err != nil || len(seen) > 256 {
			return 0, info, errBad
		}
		seen[o] = true
		c, err := x.bytes(o, 2)
		if err != nil {
			return 0, info, err
		}
		cnt := u16(c)
		if cnt == 0 || cnt > 1000 {
			return 0, info, errBad
		}
		es, err := x.bytes(o+2, int(cnt*12+4))
		if err != nil {
			return 0, info, err
		}
		end = max(end, o+2+cnt*12+4)
		tags := map[int64][]int64{}
		for i := int64(0); i < cnt; i++ {
			e := es[i*12:]
			tag, typ, count := u16(e), u16(e[2:]), u32(e[4:])
			if typ < 1 || typ > 18 || tiffTypeSize[typ] == 0 {
				return 0, info, errBad
			}
			if total := tiffTypeSize[typ] * count; total > 4 {
				end = max(end, u32(e[8:])+total)
			}
			switch tag {
			case 271:
				if ifd == 0 && count > 0 && count < 64 {
					raw := e[8:12]
					if count > 4 {
						raw, _ = x.bytes(u32(e[8:]), int(count))
					}
					info.make = strings.TrimRight(string(bytes.TrimRight(raw[:min(int(count), len(raw))], "\x00")), " ")
				}
			case 50706:
				info.dng = true
			case 273, 279, 324, 325, 513, 514, 330, 34665:
				if typ != 3 && typ != 4 && typ != 13 {
					continue
				}
				v, err := values(e, typ, count)
				if err != nil {
					return 0, info, err
				}
				tags[tag] = v
			}
		}
		for _, pair := range [][2]int64{{273, 279}, {324, 325}, {513, 514}} {
			offs, lens := tags[pair[0]], tags[pair[1]]
			for i := 0; i < len(offs) && i < len(lens); i++ {
				end = max(end, offs[i]+lens[i])
			}
		}
		queue = append(queue, tags[330]...)
		queue = append(queue, tags[34665]...)
		queue = append(queue, u32(es[cnt*12:]))
	}
	return end, info, nil
}

// RAF (Fujifilm): offsets in the header.
var rafFormat = Format{HeaderSized: true, Name: "raf", Ext: "raf", Cat: profile.Photos, MaxSize: 512 * mib,
	Magics: []Magic{m(0, "FUJIFILMCCD-RAW ")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 108)
		if err != nil {
			return 0, err
		}
		end := int64(108)
		for _, o := range []int{84, 92, 100} {
			if off, n := be32(h[o:]), be32(h[o+4:]); off > 0 && n > 0 {
				end = max(end, off+n)
			}
		}
		if _, err := x.bytes(end-1, 1); err != nil {
			return 0, err
		}
		return end, nil
	})}
