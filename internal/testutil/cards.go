// Package testutil builds fixture cards (folders and FAT images) for tests. Test use only.
package testutil

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/fat32"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// Expect is what a fixture file should be classified as.
type Expect struct {
	Cat      profile.Category
	Suggest  string
	Fragment bool
	Header   string
}

// File is one fixture file.
type File struct {
	Data []byte
	Expect
}

// Card is a fixture card's files by "/"-separated relative path.
type Card map[string]File

// Content generators: real file signatures followed by deterministic filler.
func filler(n int, tag string) []byte {
	if n <= 0 {
		return []byte{}
	}
	return bytes.Repeat([]byte(tag+"\n"), n/(len(tag)+1)+1)[:n]
}

func withHead(head []byte, n int, tag string) []byte {
	b := filler(n, tag)
	copy(b, head)
	return b
}

func JPEG(n int, tag string) []byte {
	return withHead([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1}, n, tag)
}
func PNG(n int, tag string) []byte {
	return withHead([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), n, tag)
}
func HEIC(n int, tag string) []byte {
	return withHead([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00heicmif1"), n, tag)
}
func MP4(n int, tag string) []byte {
	return withHead([]byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isommp41"), n, tag)
}
func PDF(n int, tag string) []byte { return withHead([]byte("%PDF-1.4\n"), n, tag) }
func ZIP(n int, tag string) []byte { return withHead([]byte("PK\x03\x04\x14\x00"), n, tag) }
func MP3(n int, tag string) []byte { return withHead([]byte{0xFF, 0xFB, 0x90, 0x64}, n, tag) }
func Random(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

const MiB = 1 << 20

func f(data []byte, cat profile.Category, suggest string) File {
	return File{Data: data, Expect: Expect{Cat: cat, Suggest: suggest}}
}

// AndroidCard is the plan's Android fixture. ascii=true leaves out the names the FAT image
// writer can't produce (dot-folders, Devanagari).
func AndroidCard(ascii bool) Card {
	c := Card{
		"DCIM/Camera/IMG_20240101_101010.jpg": f(JPEG(40_000, "cam1"), profile.Photos, ""),
		"DCIM/Camera/IMG_20240102_101010.jpg": f(HEIC(30_000, "heic"), profile.Photos, ""),
		"DCIM/Camera/VID_20240103.mp4":        f(MP4(MiB+3, "vid"), profile.Videos, ""),
		"Download/exact1m.bin":                f(filler(MiB, "one"), profile.Other, ""),
		"Download/exact1m1.bin":               f(filler(MiB+1, "oneplus"), profile.Other, ""),
		"Download/p4097.pdf":                  f(PDF(4097, "pdf"), profile.Documents, ""),
		"LOST.DIR/1111":                       {Data: PNG(3000, "png"), Expect: Expect{Cat: profile.Photos, Fragment: true}},
		"LOST.DIR/2222":                       {Data: Random(5000, 7), Expect: Expect{Cat: profile.Other, Fragment: true}},
		"LOST.DIR/3333":                       {Data: []byte("tiny!"), Expect: Expect{Cat: profile.Other, Fragment: true}},
		"FOUND.000/FILE0000.CHK":              {Data: JPEG(2000, "chk"), Expect: Expect{Cat: profile.Photos, Suggest: "chkdsk-fragments", Fragment: true}},
		"Android/data/com.x/cache.jpg":        f(JPEG(1000, "cache"), profile.Photos, "android-app-data"),
		"Android/media/com.whatsapp/WhatsApp/Media/WhatsApp Images/IMG-1.jpg": f(JPEG(1500, "wa"), profile.Photos, ""),
		"WhatsApp/Databases/msgstore.db.crypt14":                              f(Random(3000, 1), profile.ChatBackups, ""),
		"WhatsApp/Backups/wa.db.crypt14":                                      f(Random(2000, 2), profile.ChatBackups, ""),
		"Docs/report.pdf":                                                     f(PDF(6000, "doc"), profile.Documents, ""),
		"Docs/bundle.zip":                                                     f(ZIP(7000, "zip"), profile.Archives, ""),
		"Music/song.mp3":                                                      f(MP3(9000, "mp3"), profile.Music, ""),
		"Docs/" + strings.Repeat("x", 200) + ".txt":                           f(filler(100, "long"), profile.Documents, ""),
		"notes.ini":                        f(filler(50, "ini"), profile.Other, ""),
		"Telegram/Telegram Images/tg1.jpg": f(JPEG(1300, "tg"), profile.Photos, ""),
		"Android/media/org.telegram.messenger/Telegram/Telegram Video/tgv.mp4": f(MP4(4000, "tgv"), profile.Videos, ""),
		"WhatsApp/Media/WhatsApp Voice Notes/PTT-1.opus":                       f(withHead([]byte("OggS"), 900, "ptt"), profile.Music, ""),
		"Download/empty.pdf": {Data: []byte{}, Expect: Expect{Cat: profile.Documents, Header: "empty"}},
	}
	if !ascii { // names go-diskfs's FAT writer can't create (dot-folders, Devanagari)
		c["DCIM/.thumbnails/t1.jpg"] = f(JPEG(800, "thumb"), profile.Photos, "thumbnails")
		c["New folder/.nomedia/Af vault/v1.mp4"] = f(MP4(2*MiB+3, "vault"), profile.Videos, "")
		c["Sounds/.pictures/0.jpg"] = File{Data: []byte{}, Expect: Expect{Cat: profile.Photos, Header: "empty"}}
		c["Pictures/नमस्ते दुनिया.jpg"] = f(JPEG(1200, "hindi"), profile.Photos, "")
	}
	return c
}

// CameraCard is a DCF camera card.
func CameraCard() Card {
	return Card{
		"DCIM/100CANON/IMG_0001.JPG": f(JPEG(20_000, "c1"), profile.Photos, ""),
		"DCIM/100CANON/IMG_0001.CR3": f(withHead([]byte("\x00\x00\x00\x18ftypcrx "), 50_000, "cr3"), profile.Photos, ""),
		"DCIM/100CANON/MVI_0002.MP4": f(MP4(60_000, "mvi"), profile.Videos, ""),
		"CANONMSC/M0100.CTG":         f(filler(100, "ctg"), profile.Other, "camera-cache"),
		"MISC/AUTPRINT.MRK":          f(filler(10, "mrk"), profile.Other, ""),
	}
}

// GoProCard has low-res proxies and thumbnails that follow their video.
func GoProCard() Card {
	return Card{
		"DCIM/100GOPRO/GX010001.MP4": f(MP4(80_000, "gx"), profile.Videos, ""),
		"DCIM/100GOPRO/GX010001.LRV": f(MP4(8_000, "lrv"), profile.Videos, ""),
		"DCIM/100GOPRO/GX010001.THM": f(JPEG(500, "thm"), profile.Videos, ""),
	}
}

// DJICard has subtitles and low-res files that follow their video.
func DJICard() Card {
	return Card{
		"DCIM/100MEDIA/DJI_0001.MP4": f(MP4(70_000, "dji"), profile.Videos, ""),
		"DCIM/100MEDIA/DJI_0001.SRT": f(filler(300, "srt"), profile.Videos, ""),
		"DCIM/100MEDIA/DJI_0001.LRF": f(MP4(5000, "lrf"), profile.Videos, ""),
		"DCIM/100MEDIA/DJI_0002.JPG": f(JPEG(9000, "djp"), profile.Photos, ""),
	}
}

// AVCHDCard keeps the whole BDMV tree as Videos so it stays playable.
func AVCHDCard() Card {
	return Card{
		"PRIVATE/AVCHD/BDMV/INDEX.BDM":         f(filler(100, "idx"), profile.Videos, ""),
		"PRIVATE/AVCHD/BDMV/MOVIEOBJ.BDM":      f(filler(100, "mob"), profile.Videos, ""),
		"PRIVATE/AVCHD/BDMV/STREAM/00000.MTS":  f(Random(19_200, 3), profile.Videos, ""),
		"PRIVATE/AVCHD/BDMV/CLIPINF/00000.CPI": f(filler(100, "cpi"), profile.Videos, ""),
	}
}

// Fixed modification time for fixture files (FAT stores 2-second resolution).
var MTime = time.Date(2024, 5, 6, 7, 8, 10, 0, time.Local)

// WriteFolder writes card into a new temp folder and returns it.
func WriteFolder(t testing.TB, card Card) string {
	t.Helper()
	dir := t.TempDir()
	for rel, fl := range card {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, fl.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, MTime, MTime)
	}
	return dir
}

// imageSerial gives every fixture image its own volume serial, like real cards: the per-card lock
// is machine-wide and keyed by it, so test packages running in parallel never share a "card".
var imageSerial atomic.Uint32

// FAT32Image writes card into a superfloppy FAT32 image (ASCII names only) and returns its path.
func FAT32Image(t testing.TB, card Card, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "card.img")
	writeFAT32(t, p, card, size)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	serial := uint32(time.Now().UnixNano())*2654435761 ^ uint32(os.Getpid())<<16 ^ imageSerial.Add(1)
	if _, err := f.WriteAt([]byte{byte(serial), byte(serial >> 8), byte(serial >> 16), byte(serial >> 24)}, 67); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFAT32(t testing.TB, p string, card Card, size int64) {
	t.Helper()
	b, err := file.CreateFromPath(p, size)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	fsys, err := fat32.Create(b, size, 0, 512, "CARD", true)
	if err != nil {
		t.Fatal(err)
	}
	for rel, fl := range card {
		if d := path.Dir(rel); d != "." {
			if err := fsys.Mkdir("/" + d); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		w, err := fsys.OpenFile("/"+rel, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("create %s: %v", rel, err)
		}
		if _, err := w.Write(fl.Data); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		w.Close()
	}
}
