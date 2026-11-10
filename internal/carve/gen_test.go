package carve

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

// Generators for every format the carver sizes. Each returns one complete, valid file.

func testImage(w, h int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(int64(w*h + 1)))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{uint8(r.Intn(256)), uint8(x), uint8(y), 255})
		}
	}
	return m
}

func genJPEG(t testing.TB) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(64, 48), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	// add an APP1 segment holding a fake embedded JPEG (EXIF thumbnail): its FFD9 must not end the file
	data := b.Bytes()
	thumb := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	app1 := append([]byte{0xFF, 0xE1, 0, byte(2 + 6 + len(thumb))}, append([]byte("Exif\x00\x00"), thumb...)...)
	return append(append(append([]byte{}, data[:2]...), app1...), data[2:]...)
}

func genPNG(t testing.TB) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(40, 30)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func genGIF(t testing.TB) []byte {
	var b bytes.Buffer
	pm := image.NewPaletted(image.Rect(0, 0, 30, 20), []color.Color{color.Black, color.White, color.RGBA{255, 0, 0, 255}})
	for i := range pm.Pix {
		pm.Pix[i] = uint8(i % 3)
	}
	g := &gif.GIF{Image: []*image.Paletted{pm, pm}, Delay: []int{5, 5}}
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func genBMP(t testing.TB) []byte {
	var b bytes.Buffer
	if err := bmp.Encode(&b, testImage(20, 10)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func genTIFF(t testing.TB) []byte {
	var b bytes.Buffer
	if err := tiff.Encode(&b, testImage(32, 32), &tiff.Options{Compression: tiff.Deflate}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// genNEF is a little-endian TIFF whose Make is NIKON, with its strip data placed after the IFD.
func genNEF(t testing.TB) []byte {
	le := binary.LittleEndian
	b := []byte("II*\x00\x08\x00\x00\x00")
	make_ := []byte("NIKON CORPORATION\x00")
	const n = 4
	ifdSize := 2 + n*12 + 4
	makeOff := 8 + ifdSize
	stripOff := makeOff + len(make_)
	strip := bytes.Repeat([]byte{0xAB}, 3000)
	ifd := make([]byte, ifdSize)
	le.PutUint16(ifd, n)
	put := func(i int, tag, typ uint16, count, val uint32) {
		e := ifd[2+i*12:]
		le.PutUint16(e, tag)
		le.PutUint16(e[2:], typ)
		le.PutUint32(e[4:], count)
		le.PutUint32(e[8:], val)
	}
	put(0, 256, 4, 1, 32)                               // width
	put(1, 271, 2, uint32(len(make_)), uint32(makeOff)) // Make
	put(2, 273, 4, 1, uint32(stripOff))                 // StripOffsets
	put(3, 279, 4, 1, uint32(len(strip)))               // StripByteCounts
	b = append(b, ifd...)
	b = append(b, make_...)
	return append(b, strip...)
}

func genRAF(testing.TB) []byte {
	b := make([]byte, 160)
	copy(b, "FUJIFILMCCD-RAW 0201FF383501")
	be := binary.BigEndian
	be.PutUint32(b[84:], 160)  // JPEG offset
	be.PutUint32(b[88:], 1000) // JPEG length
	be.PutUint32(b[100:], 1160)
	be.PutUint32(b[104:], 4000) // CFA
	return append(b, bytes.Repeat([]byte{7}, 5000)...)
}

func riff(form string, chunks ...[]byte) []byte {
	var body []byte
	body = append(body, form...)
	for _, c := range chunks {
		body = append(body, c...)
	}
	h := []byte("RIFF\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(h[4:], uint32(len(body)))
	return append(h, body...)
}

func chunkLE(id string, data []byte) []byte {
	h := make([]byte, 8)
	copy(h, id)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(data)))
	c := append(h, data...)
	if len(data)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

func genWEBP(testing.TB) []byte {
	return riff("WEBP", chunkLE("VP8 ", bytes.Repeat([]byte{1}, 301)))
}

func genWAV(testing.TB) []byte {
	return riff("WAVE", chunkLE("fmt ", make([]byte, 16)), chunkLE("data", bytes.Repeat([]byte{3}, 4000)))
}

func genAVI(testing.TB) []byte {
	a := riff("AVI ", chunkLE("LIST", []byte("hdrl")), chunkLE("LIST", append([]byte("movi"), make([]byte, 2000)...)))
	x := riff("AVIX", chunkLE("LIST", append([]byte("movi"), make([]byte, 1000)...)))
	return append(a, x...)
}

func genAIFF(testing.TB) []byte {
	ck := func(id string, d []byte) []byte {
		h := make([]byte, 8)
		copy(h, id)
		binary.BigEndian.PutUint32(h[4:], uint32(len(d)))
		return append(h, d...)
	}
	body := append([]byte("AIFF"), ck("COMM", make([]byte, 18))...)
	body = append(body, ck("SSND", make([]byte, 3000))...)
	return ck("FORM", body)
}

func box(typ string, payload []byte) []byte {
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(8+len(payload)))
	copy(h[4:], typ)
	return append(h, payload...)
}

func genBMFF(brand string, needs ...string) []byte {
	b := box("ftyp", append([]byte(brand), 0, 0, 0, 0))
	for _, n := range needs {
		b = append(b, box(n, bytes.Repeat([]byte{9}, 700))...)
	}
	return b
}

func genMP4(testing.TB) []byte  { return genBMFF("isom", "moov", "mdat") }
func genHEIC(testing.TB) []byte { return genBMFF("heic", "meta", "mdat") }

func vint(n int) []byte { // 8-byte EBML size
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(n))
	b[0] = 0x01
	return b
}

func genMKV(testing.TB) []byte {
	doc := append([]byte{0x42, 0x82, 0x84}, "webm"...)
	hdr := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, vint(len(doc))...)
	hdr = append(hdr, doc...)
	cluster := append([]byte{0x1F, 0x43, 0xB6, 0x75}, vint(1500)...)
	cluster = append(cluster, make([]byte, 1500)...)
	seg := append([]byte{0x18, 0x53, 0x80, 0x67}, vint(len(cluster))...)
	return append(hdr, append(seg, cluster...)...)
}

// genMKVUnknownSize has a Segment of unknown size: its end is found by walking its children.
func genMKVUnknownSize(testing.TB) []byte {
	doc := append([]byte{0x42, 0x82, 0x88}, "matroska"...)
	hdr := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, vint(len(doc))...)
	hdr = append(hdr, doc...)
	seg := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	for i := 0; i < 3; i++ {
		seg = append(seg, append([]byte{0x1F, 0x43, 0xB6, 0x75}, vint(800)...)...)
		seg = append(seg, make([]byte, 800)...)
	}
	return append(hdr, seg...)
}

func genTS(testing.TB) []byte {
	var b []byte
	for i := 0; i < 40; i++ {
		p := make([]byte, 188)
		p[0] = 0x47
		p[1] = byte(i)
		b = append(b, p...)
	}
	return b
}

func genM2TS(testing.TB) []byte {
	var b []byte
	for i := 0; i < 40; i++ {
		p := make([]byte, 192)
		p[4] = 0x47
		b = append(b, p...)
	}
	return b
}

func genMPG(testing.TB) []byte {
	pack := []byte{0, 0, 1, 0xBA, 0x44, 0, 4, 0, 4, 1, 1, 0x89, 0xC3, 0xF8} // MPEG-2, no stuffing
	pes := append([]byte{0, 0, 1, 0xE0, 0x03, 0xE8}, make([]byte, 1000)...)
	var b []byte
	for i := 0; i < 5; i++ {
		b = append(b, pack...)
		b = append(b, pes...)
	}
	return append(b, 0, 0, 1, 0xB9)
}

func genMP3(testing.TB) []byte {
	id3 := []byte("ID3\x03\x00\x00\x00\x00\x00\x0A")
	id3 = append(id3, make([]byte, 10)...)
	frame := make([]byte, 417) // MPEG1 L3 128 kbps 44.1 kHz, no padding: 144*128000/44100 = 417
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
	b := id3
	for i := 0; i < 20; i++ {
		b = append(b, frame...)
	}
	tag := make([]byte, 128)
	copy(tag, "TAG")
	return append(b, tag...)
}

func genAAC(testing.TB) []byte {
	var b []byte
	for i := 0; i < 20; i++ {
		n := 300
		f := make([]byte, n)
		f[0], f[1] = 0xFF, 0xF1
		f[2] = 0x50
		f[3] = 0x80 | byte(n>>11)
		f[4] = byte(n >> 3)
		f[5] = byte(n<<5) | 0x1F
		f[6] = 0xFC
		b = append(b, f...)
	}
	return b
}

func genAMR(testing.TB) []byte {
	b := []byte("#!AMR\n")
	for i := 0; i < 50; i++ {
		f := make([]byte, 32) // FT 7 (12.2 kbps): 31 bytes + header
		f[0] = 7<<3 | 0x04
		b = append(b, f...)
	}
	return b
}

func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for i := 0; i < 8; i++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func flacFrame(n int, payload []byte) []byte {
	h := []byte{0xFF, 0xF8, 0xC9, 0x08, byte(n)} // block size 4096 (code 12 → 256<<4), 44.1 kHz, mono 16-bit
	h = append(h, crc8(h))
	f := append(h, payload...)
	c := crc16(f)
	return append(f, byte(c>>8), byte(c))
}

func genFLAC(testing.TB) []byte {
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], 4096)
	binary.BigEndian.PutUint16(si[2:], 4096)
	// total samples = 3 frames × 4096 (36-bit field at the end of byte 13..17)
	total := uint64(3 * 4096)
	si[13] = byte(total >> 32 & 0x0F)
	binary.BigEndian.PutUint32(si[14:], uint32(total))
	b := append([]byte("fLaC"), 0x80, 0, 0, 34)
	b = append(b, si...)
	r := rand.New(rand.NewSource(5))
	for i := 0; i < 3; i++ {
		p := make([]byte, 700)
		r.Read(p)
		for j := range p { // keep the payload free of frame syncs
			if p[j] == 0xFF {
				p[j] = 0xFE
			}
		}
		b = append(b, flacFrame(i, p)...)
	}
	return b
}

func oggPage(typ byte, payload []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	h[5] = typ
	var segs []byte
	n := len(payload)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	segs = append(segs, byte(n))
	h[26] = byte(len(segs))
	return append(append(h, segs...), payload...)
}

func genOGG(testing.TB) []byte {
	b := oggPage(0x02, append([]byte("OpusHead"), make([]byte, 11)...))
	b = append(b, oggPage(0, make([]byte, 600))...)
	return append(b, oggPage(0x04, make([]byte, 100))...)
}

func genMIDI(testing.TB) []byte {
	b := []byte("MThd\x00\x00\x00\x06\x00\x01\x00\x02\x01\xE0")
	for i := 0; i < 2; i++ {
		trk := []byte{0x00, 0xFF, 0x2F, 0x00}
		h := []byte("MTrk\x00\x00\x00\x00")
		binary.BigEndian.PutUint32(h[4:], uint32(len(trk)))
		b = append(append(b, h...), trk...)
	}
	return b
}

func genPDF(testing.TB) []byte {
	return []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n2 0 obj<<>>endobj\n%%EOF\r\n")
}

func genZIP(t testing.TB) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, n := range []string{"word/document.xml", "[Content_Types].xml"} {
		f, _ := w.Create(n) // data descriptors: sizes come after the data
		f.Write(bytes.Repeat([]byte("hello docx "), 50))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func genZIPStored(t testing.TB) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	data := []byte("plain archive member")
	f, err := w.CreateRaw(&zip.FileHeader{Name: "a.txt", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data),
		CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	w.Close()
	return b.Bytes()
}

func gen7z(testing.TB) []byte {
	b := make([]byte, 32)
	copy(b, "7z\xBC\xAF\x27\x1C\x00\x04")
	binary.LittleEndian.PutUint64(b[12:], 2000) // next header offset (after the 32-byte start header)
	binary.LittleEndian.PutUint64(b[20:], 100)  // next header size
	return append(b, make([]byte, 2100)...)
}

func genRAR4(testing.TB) []byte {
	b := []byte("Rar!\x1A\x07\x00")
	main := []byte{0, 0, 0x73, 0, 0, 13, 0, 0, 0, 0, 0, 0, 0}
	file := []byte{0, 0, 0x74, 0x00, 0x80, 32, 0} // LONG_BLOCK: ADD_SIZE follows
	file = append(file, 0xE8, 0x03, 0, 0)         // 1000 bytes of data
	file = append(file, make([]byte, 32-len(file))...)
	file = append(file, make([]byte, 1000)...)
	end := []byte{0, 0, 0x7B, 0, 0, 7, 0}
	return append(append(append(b, main...), file...), end...)
}

func genRAR5(testing.TB) []byte {
	b := []byte("Rar!\x1A\x07\x01\x00")
	blk := func(typ, flags byte, data int) []byte {
		h := []byte{typ, flags}
		if flags&2 != 0 {
			h = append(h, byte(data&0x7F|0x80), byte(data>>7))
		}
		out := append([]byte{0, 0, 0, 0, byte(len(h))}, h...)
		return append(out, make([]byte, data)...)
	}
	b = append(b, blk(1, 0, 0)...)
	b = append(b, blk(2, 2, 900)...)
	return append(b, blk(5, 0, 0)...)
}

func genGZ(t testing.TB) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Name = "x.txt"
	r := rand.New(rand.NewSource(3))
	p := make([]byte, 20000)
	r.Read(p)
	w.Write(p)
	w.Close()
	return b.Bytes()
}

func genSQLite(testing.TB) []byte {
	b := make([]byte, 4096*3)
	copy(b, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(b[16:], 4096)
	b[18], b[19], b[21], b[22], b[23] = 1, 1, 64, 32, 32
	binary.BigEndian.PutUint32(b[24:], 7) // change counter
	binary.BigEndian.PutUint32(b[28:], 3) // page count
	binary.BigEndian.PutUint32(b[92:], 7) // version-valid-for
	return b
}

// genOLE2: 512-byte sectors; sector 0 = FAT, sector 1 = directory ("WordDocument"), sectors 2..4 data.
func genOLE2(testing.TB) []byte {
	le := binary.LittleEndian
	h := make([]byte, 512)
	copy(h, "\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1")
	le.PutUint16(h[26:], 3)
	le.PutUint16(h[28:], 0xFFFE)
	le.PutUint16(h[30:], 9)
	le.PutUint16(h[32:], 6)
	le.PutUint32(h[44:], 1)          // one FAT sector
	le.PutUint32(h[48:], 1)          // directory at sector 1
	le.PutUint32(h[60:], 0xFFFFFFFE) // no mini FAT
	le.PutUint32(h[68:], 0xFFFFFFFE) // no DIFAT sectors
	for i := 0; i < 109; i++ {
		le.PutUint32(h[76+i*4:], 0xFFFFFFFF)
	}
	le.PutUint32(h[76:], 0) // FAT is sector 0
	fat := make([]byte, 512)
	for i := 0; i < 128; i++ {
		le.PutUint32(fat[i*4:], 0xFFFFFFFF)
	}
	le.PutUint32(fat[0:], 0xFFFFFFFD)  // FAT sector
	le.PutUint32(fat[4:], 0xFFFFFFFE)  // directory: end of chain
	le.PutUint32(fat[8:], 3)           // data 2 → 3 → 4
	le.PutUint32(fat[12:], 4)          //
	le.PutUint32(fat[16:], 0xFFFFFFFE) //
	dir := make([]byte, 512)
	name := "WordDocument"
	for i, c := range name {
		le.PutUint16(dir[i*2:], uint16(c))
	}
	le.PutUint16(dir[64:], uint16(2*(len(name)+1)))
	dir[66] = 2
	data := bytes.Repeat([]byte{0x5A}, 3*512)
	return append(append(append(h, fat...), dir...), data...)
}

func genRTF(testing.TB) []byte {
	return []byte(`{\rtf1\ansi{\fonttbl{\f0 Arial;}}\f0 Hello \{not a group\} {\b bold} world}`)
}

func genFLV(testing.TB) []byte {
	b := []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}
	for i := 0; i < 5; i++ {
		data := make([]byte, 200)
		tag := []byte{9, 0, 0, 200, 0, 0, byte(i), 0, 0, 0, 0}
		b = append(append(b, tag...), data...)
		prev := make([]byte, 4)
		binary.BigEndian.PutUint32(prev, uint32(11+len(data)))
		b = append(b, prev...)
	}
	return b
}

var (
	guidHeader = []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	guidData   = []byte{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
)

func genASF(testing.TB) []byte {
	obj := func(g []byte, payload int) []byte {
		b := append([]byte{}, g...)
		s := make([]byte, 8)
		binary.LittleEndian.PutUint64(s, uint64(24+payload))
		return append(append(b, s...), make([]byte, payload)...)
	}
	return append(obj(guidHeader, 300), obj(guidData, 3000)...)
}

// generators: format name → (generator, expected extension).
var generators = []struct {
	format, ext string
	gen         func(testing.TB) []byte
}{
	{"jpeg", "jpg", genJPEG}, {"png", "png", genPNG}, {"gif", "gif", genGIF}, {"bmp", "bmp", genBMP},
	{"tiff", "tif", genTIFF}, {"tiff", "nef", genNEF}, {"raf", "raf", genRAF},
	{"riff", "webp", genWEBP}, {"riff", "wav", genWAV}, {"riff", "avi", genAVI}, {"aiff", "aiff", genAIFF},
	{"isobmff", "mp4", genMP4}, {"isobmff", "heic", genHEIC},
	{"ebml", "webm", genMKV}, {"ebml", "mkv", genMKVUnknownSize},
	{"mpegts", "ts", genTS}, {"mpegts", "m2ts", genM2TS}, {"mpegps", "mpg", genMPG},
	{"mp3", "mp3", genMP3}, {"aac", "aac", genAAC}, {"amr", "amr", genAMR}, {"flac", "flac", genFLAC},
	{"ogg", "opus", genOGG}, {"midi", "mid", genMIDI}, {"pdf", "pdf", genPDF},
	{"zip", "docx", genZIP}, {"zip", "zip", genZIPStored}, {"7z", "7z", gen7z},
	{"rar", "rar", genRAR4}, {"rar", "rar", genRAR5}, {"gzip", "gz", genGZ},
	{"sqlite", "sqlite", genSQLite}, {"ole2", "doc", genOLE2}, {"rtf", "rtf", genRTF},
	{"flv", "flv", genFLV}, {"asf", "wmv", genASF},
}
