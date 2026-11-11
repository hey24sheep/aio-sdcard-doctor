package carve

import (
	"io"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

var mp3Bitrates = [2][16]int64{
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}, // MPEG-1 Layer III
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},     // MPEG-2/2.5 Layer III
}

// mp3FrameLen returns the length of the MPEG audio Layer III frame starting with b, or 0.
func mp3FrameLen(b []byte) int64 {
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 || (b[1]>>1)&3 != 1 {
		return 0
	}
	ver := (b[1] >> 3) & 3 // 0 = 2.5, 2 = 2, 3 = 1
	bri, sri, pad := b[2]>>4, (b[2]>>2)&3, int64(b[2]>>1&1)
	if ver == 1 || sri == 3 {
		return 0
	}
	sr := [3]int64{44100, 48000, 32000}[sri]
	if ver == 3 {
		br := mp3Bitrates[0][bri]
		if br == 0 {
			return 0
		}
		return 144000*br/sr + pad
	}
	br := mp3Bitrates[1][bri]
	if br == 0 {
		return 0
	}
	if ver == 2 {
		sr /= 2
	} else {
		sr /= 4
	}
	return 72000*br/sr + pad
}

// MP3: ID3v2 size + frame walk (+ ID3v1 tag).
var mp3Format = Format{Stream: true, Name: "mp3", Ext: "mp3", Cat: profile.Music, MaxSize: 1 * gib,
	Magics: []Magic{m(0, "ID3"), {0, []byte{0xFF, 0xFB}}, {0, []byte{0xFF, 0xFA}}, {0, []byte{0xFF, 0xF3}},
		{0, []byte{0xFF, 0xF2}}, {0, []byte{0xFF, 0xE3}}, {0, []byte{0xFF, 0xE2}}},
	Size: streamSizer(func(x *rd) (int64, error) {
		pos, need := int64(0), 8
		h, err := x.bytes(0, 10)
		if err != nil {
			return 0, err
		}
		if string(h[:3]) == "ID3" {
			var n int64
			for _, c := range h[6:10] {
				if c >= 0x80 {
					return 0, errBad
				}
				n = n<<7 | int64(c)
			}
			pos = 10 + n
			if h[5]&0x10 != 0 {
				pos += 10
			}
			need = 4
		}
		frames := 0
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 4)
			if err != nil {
				return 0, err
			}
			l := mp3FrameLen(b)
			if l < 4 {
				break
			}
			pos += l
			frames++
		}
		if frames < need {
			return 0, errBad
		}
		if t, err := x.bytes(pos, 3); err == nil && string(t) == "TAG" {
			if _, err := x.bytes(pos+127, 1); err == nil {
				pos += 128
			}
		}
		return pos, nil
	})}

// AAC (ADTS): frame lengths.
var aacFormat = Format{Stream: true, Name: "aac", Ext: "aac", Cat: profile.Music, MaxSize: 1 * gib,
	Magics: []Magic{{0, []byte{0xFF, 0xF1}}, {0, []byte{0xFF, 0xF9}}}, Size: streamSizer(func(x *rd) (int64, error) {
		pos, frames := int64(0), 0
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 7)
			if err != nil {
				return 0, err
			}
			l := int64(b[3]&3)<<11 | int64(b[4])<<3 | int64(b[5]>>5)
			if b[0] != 0xFF || b[1]&0xF6 != 0xF0 || (b[2]>>2)&0xF > 12 || l < 7 {
				break
			}
			pos += l
			frames++
		}
		if frames < 8 {
			return 0, errBad
		}
		return pos, nil
	})}

var amrSizes = [2][16]int64{
	{12, 13, 15, 17, 19, 20, 26, 31, 5, 6, 5, 5, -1, -1, -1, 0},
	{17, 23, 32, 36, 40, 46, 50, 58, 60, 5, -1, -1, -1, -1, 0, 0},
}

// AMR / AMR-WB: frame headers; ends at the first invalid header or an all-zero frame.
var amrFormat = Format{Stream: true, Name: "amr", Ext: "amr", Cat: profile.Music, MaxSize: 256 * mib,
	Magics: []Magic{m(0, "#!AMR\n"), m(0, "#!AMR-WB\n")}, Size: streamSizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 9)
		if err != nil {
			return 0, err
		}
		wb, pos := 0, int64(6)
		if string(h) == "#!AMR-WB\n" {
			wb, pos = 1, 9
		}
		frames := 0
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 1)
			if err != nil {
				return 0, err
			}
			n := amrSizes[wb][b[0]>>3&0xF]
			if b[0]&0x83 != 0 || n < 0 {
				break
			}
			f, err := x.bytes(pos, int(1+n))
			if err != nil {
				return 0, err
			}
			if allZero(f) {
				break
			}
			pos += 1 + n
			frames++
		}
		if frames == 0 {
			return 0, errBad
		}
		return pos, nil
	})}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i) << 8
		for j := 0; j < 8; j++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

// crc16 is FLAC's frame CRC (polynomial 0x8005, initial 0).
func crc16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c = c<<8 ^ crc16Table[byte(c>>8)^x]
	}
	return c
}

func flacBlockSize(h []byte) int64 {
	code := int64(h[2] >> 4)
	switch {
	case code == 1:
		return 192
	case code >= 2 && code <= 5:
		return 576 << (code - 2)
	case code >= 8:
		return 256 << (code - 8)
	case code == 6 || code == 7:
		n := 1 // UTF-8 coded frame number length
		for mask := byte(0x80); h[4]&mask != 0 && n < 7; mask >>= 1 {
			n++
		}
		if n > 1 {
			n--
		}
		if 4+n+2 > len(h) {
			return 0
		}
		if code == 6 {
			return int64(h[4+n]) + 1
		}
		return be16(h[4+n:]) + 1
	}
	return 0
}

// FLAC: metadata blocks, then frames found by their CRC-16 footers and the next frame's sync,
// up to STREAMINFO's total sample count.
var flacFormat = Format{Name: "flac", Ext: "flac", Cat: profile.Music, MaxSize: 2 * gib,
	Magics: []Magic{m(0, "fLaC")}, Size: sizer(func(x *rd) (int64, error) {
		pos, total, maxFrame := int64(4), int64(0), int64(0)
		for i := 0; ; i++ {
			if err := x.step(); err != nil {
				return 0, err
			}
			b, err := x.bytes(pos, 4)
			if err != nil {
				return 0, err
			}
			typ, n := b[0]&0x7F, be32(b)&0xFFFFFF
			if typ == 127 || i == 0 && (typ != 0 || n != 34) {
				return 0, errBad
			}
			if typ == 0 {
				si, err := x.bytes(pos+4, 34)
				if err != nil {
					return 0, err
				}
				maxFrame = be32(si[6:]) & 0xFFFFFF
				total = int64(si[13]&0x0F)<<32 | be32(si[14:])
			}
			pos += 4 + n
			if b[0]&0x80 != 0 {
				break
			}
		}
		frameCap := 16 * mib
		if maxFrame > 0 {
			frameCap = maxFrame + 64
		}
		samples, frames := int64(0), 0
		for {
			h, err := x.bytes(pos, 16)
			if err != nil {
				if frames > 0 && total == 0 {
					return pos, nil
				}
				return 0, err
			}
			if h[0] != 0xFF || h[1]&0xFE != 0xF8 {
				if frames > 0 {
					return pos, nil
				}
				return 0, errBad
			}
			bs := flacBlockSize(h)
			if bs == 0 {
				return 0, errBad
			}
			start, q, c := pos, pos, uint16(0)
			next := int64(-1)
			for next < 0 {
				if err := x.step(); err != nil {
					return 0, err
				}
				if q-start > frameCap {
					return 0, errBad
				}
				n := min(64*kib, x.limit-q)
				if n <= 0 {
					x.seen = x.limit
					return 0, errTrunc
				}
				b, err := x.bytes(q, int(n))
				if err != nil {
					return 0, err
				}
				for i, v := range b {
					c = c<<8 ^ crc16Table[byte(c>>8)^v]
					end := q + int64(i) + 1
					if c != 0 || end-start < 16 {
						continue
					}
					if total > 0 && samples+bs >= total {
						return end, nil
					}
					var nx []byte
					if i+2 < len(b) {
						nx = b[i+1 : i+3]
					} else if nx, err = x.bytes(end, 2); err != nil {
						continue
					}
					if nx[0] == 0xFF && nx[1]&0xFE == 0xF8 {
						next = end
						break
					}
					if total == 0 {
						return end, nil
					}
				}
				q += n
			}
			samples += bs
			frames++
			pos = next
		}
	})}

// Ogg (Opus, Vorbis, FLAC-in-Ogg): pages to the end-of-stream page.
var oggFormat = Format{Name: "ogg", Ext: "ogg", Cat: profile.Music, MaxSize: 2 * gib,
	Magics: []Magic{m(0, "OggS")},
	Size: sizer(func(x *rd) (int64, error) {
		pos, pages, eos := int64(0), 0, false
		for {
			if err := x.step(); err != nil {
				return 0, err
			}
			h, err := x.bytes(pos, 27)
			if err != nil {
				if eos && pos == x.limit {
					return pos, nil
				}
				return 0, err
			}
			if string(h[:4]) != "OggS" || h[4] != 0 {
				if pages == 0 {
					return 0, errBad
				}
				if eos {
					return pos, nil
				}
				x.seen = pos
				return 0, errTrunc
			}
			segs, err := x.bytes(pos+27, int(h[26]))
			if err != nil {
				return 0, err
			}
			n := int64(27 + len(segs))
			for _, s := range segs {
				n += int64(s)
			}
			eos = h[5]&4 != 0
			pos += n
			pages++
			if pos > x.limit {
				x.seen = x.limit
				return 0, errTrunc
			}
		}
	}),
	Sub: func(r io.ReaderAt, off, n int64) string {
		h := make([]byte, 27+255+8)
		got, _ := r.ReadAt(h, off)
		if got < 28 || 27+int(h[26])+8 > got {
			return ""
		}
		p := h[27+int(h[26]):]
		switch {
		case string(p[:8]) == "OpusHead":
			return "opus"
		case string(p[1:5]) == "FLAC":
			return "oga"
		}
		return "ogg"
	}}

// MIDI: MThd, then the number of MTrk chunks it declares.
var midiFormat = Format{Name: "midi", Ext: "mid", Cat: profile.Music, MaxSize: 64 * mib,
	Magics: []Magic{m(0, "MThd\x00\x00\x00\x06")}, Size: sizer(func(x *rd) (int64, error) {
		h, err := x.bytes(0, 14)
		if err != nil {
			return 0, err
		}
		ntrk := be16(h[10:])
		if ntrk == 0 || be16(h[8:]) > 2 {
			return 0, errBad
		}
		pos, trk := int64(14), int64(0)
		for trk < ntrk {
			if err := x.step(); err != nil {
				return 0, err
			}
			c, err := x.bytes(pos, 8)
			if err != nil {
				return 0, err
			}
			for _, b := range c[:4] {
				if b < 0x20 || b > 0x7E {
					return 0, errBad
				}
			}
			if string(c[:4]) == "MTrk" {
				trk++
			}
			pos += 8 + be32(c[4:])
		}
		if _, err := x.bytes(pos-1, 1); err != nil {
			return 0, err
		}
		return pos, nil
	})}
