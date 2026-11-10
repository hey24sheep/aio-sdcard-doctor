package profile

import (
	"bytes"
	"testing"
	"testing/fstest"
)

func pad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(append([]byte{}, b...), make([]byte, n-len(b))...)
}

func ftyp(brand string) []byte {
	b := []byte{0, 0, 0, 0x18}
	b = append(b, "ftyp"+brand+"\x00\x00\x00\x00"+brand+"mif1"...)
	return pad(b, 64)
}

func tsPackets(prefix int) []byte {
	b := make([]byte, 600)
	for i := prefix; i < len(b); i += 188 + prefix {
		b[i] = 0x47
	}
	return b
}

var (
	jpegHead = pad([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, 64)
	pngHead  = pad([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), 64)
	gifHead  = pad([]byte("GIF89a"), 64)
	tiffHead = pad([]byte("II*\x00\x08\x00\x00\x00"), 64)
	orfHead  = pad([]byte("IIRO\x08\x00\x00\x00"), 64)
	rw2Head  = pad([]byte("IIU\x00\x18\x00\x00\x00"), 64)
	rafHead  = pad([]byte("FUJIFILMCCD-RAW 0201FF383501"), 64)
	pdfHead  = pad([]byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"), 64)
	zipHead  = pad([]byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"), 64)
	mp3Head  = pad([]byte{0xFF, 0xFB, 0x90, 0x64}, 64) // MPEG-1 Layer III frame header
)

func TestByExtTables(t *testing.T) {
	cases := map[string]Category{
		"jpg": Photos, ".JPG": Photos, "heic": Photos, "cr3": Photos, "insp": Photos, "dng": Photos,
		"mp4": Videos, "mts": Videos, "insv": Videos, "braw": Videos, "mod": Videos,
		"mp3": Music, "opus": Music, "dsf": Music,
		"pdf": Documents, "vcf": Documents, "epub": Documents,
		"zip": Archives, "7z": Archives, "tgz": Archives,
	}
	for ext, want := range cases {
		if got, ok := ByExt(ext); !ok || got != want {
			t.Errorf("ByExt(%q) = %v,%v want %v", ext, got, ok, want)
		}
	}
	for _, ext := range []string{"", "ini", "thm", "xmp", "exe"} {
		if _, ok := ByExt(ext); ok {
			t.Errorf("ByExt(%q) should be unknown", ext)
		}
	}
}

func TestSniffMimetypeAndGaps(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		cat  Category
		ext  string
	}{
		{"jpeg", jpegHead, Photos, ".jpg"},
		{"png", pngHead, Photos, ".png"},
		{"heic", ftyp("heic"), Photos, ".heic"},
		{"mp4", ftyp("isom"), Videos, ".mp4"},
		{"cr3", ftyp("crx "), Photos, ".cr3"},
		{"orf", orfHead, Photos, ".orf"},
		{"rw2", rw2Head, Photos, ".rw2"},
		{"raf", rafHead, Photos, ".raf"},
		{"ts", tsPackets(0), Videos, ".ts"},
		{"mts", tsPackets(4), Videos, ".m2ts"},
		{"pdf", pdfHead, Documents, ".pdf"},
		{"zip", zipHead, Archives, ".zip"},
		{"mp3", mp3Head, Music, ".mp3"},
	}
	for _, c := range cases {
		cat, ext, ok := Sniff(c.head)
		if !ok || cat != c.cat || ext != c.ext {
			t.Errorf("%s: Sniff = %v %q %v, want %v %q", c.name, cat, ext, ok, c.cat, c.ext)
		}
	}
	if _, _, ok := Sniff(bytes.Repeat([]byte{0x13, 0x37, 0x99}, 100)); ok {
		t.Error("random bytes sniffed as a known type")
	}
}

func TestSniffShortInput(t *testing.T) {
	for n := 0; n < 12; n++ {
		if _, _, ok := Sniff(jpegHead[:n]); ok {
			t.Errorf("Sniff of %d bytes should be unknown", n)
		}
	}
}

func TestHeaderCheck(t *testing.T) {
	cases := []struct {
		ext  string
		head []byte
		want string
	}{
		{".jpg", nil, "empty"},
		{".jpg", make([]byte, 512), "zeroed"},
		{".jpg", ftyp("heic"), "mismatch"},
		{".jpg", pngHead, "mismatch"},
		{".cr3", ftyp("crx "), "ok"},
		{".nef", tiffHead, "ok"},
		{".orf", orfHead, "ok"},
		{".mts", tsPackets(4), "ok"},
		{".ts", tsPackets(0), "ok"},
		{".txt", []byte("hello"), "n/a"},
		{".jpg", jpegHead, "ok"},
		{".JPG", jpegHead, "ok"},
		{".mov", ftyp("qt  "), "ok"},
		{".docx", zipHead, "ok"},
		{".pdf", pdfHead, "ok"},
		{".gif", gifHead, "ok"},
	}
	for _, c := range cases {
		if got := HeaderCheck(c.ext, c.head); got != c.want {
			t.Errorf("HeaderCheck(%q) = %q want %q", c.ext, got, c.want)
		}
	}
}

func names(ps []Profile) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDetectProfiles(t *testing.T) {
	f := &fstest.MapFile{Data: []byte("x")}
	cases := []struct {
		fs   fstest.MapFS
		want []string
	}{
		{fstest.MapFS{"Android/x": f, "DCIM/Camera/a.jpg": f}, []string{"Android", "Generic"}},
		{fstest.MapFS{"DCIM/100CANON/IMG_1.JPG": f}, []string{"DCFCamera", "Generic"}},
		{fstest.MapFS{"DCIM/100GOPRO/GX01.MP4": f}, []string{"DCFCamera", "GoPro", "Generic"}},
		{fstest.MapFS{"DCIM/100MEDIA/DJI_0001.MP4": f}, []string{"DCFCamera", "DJI", "Generic"}},
		{fstest.MapFS{"PRIVATE/AVCHD/BDMV/STREAM/00000.MTS": f}, []string{"AVCHD", "Generic"}},
		{fstest.MapFS{"PRIVATE/M4ROOT/CLIP/C0001.MP4": f}, []string{"SonyXAVC", "Generic"}},
		{fstest.MapFS{"DCIM/Camera01/VID_1.insv": f}, []string{"Insta360", "Generic"}},
		{fstest.MapFS{"stuff/a.txt": f}, []string{"Generic"}},
	}
	for i, c := range cases {
		if got := names(Detect(c.fs)); !eq(got, c.want) {
			t.Errorf("case %d: Detect = %v want %v", i, got, c.want)
		}
	}
}

func byName(ns ...string) []Profile {
	var out []Profile
	for _, n := range ns {
		for _, p := range Builtin {
			if p.Name == n {
				out = append(out, p)
			}
		}
	}
	return out
}

// TestDecideNeverSkips: every path gets a category; suggestion matches only tag the file.
func TestDecideNeverSkips(t *testing.T) {
	active := byName("Android", "Generic")
	for _, rel := range []string{".thumbnails/t.jpg", "Android/data/com.x/cache.jpg", "System Volume Information/x",
		"._IMG.jpg", "FOUND.000/FILE0000.CHK", "notes.ini", "a/b/c"} {
		d := Decide(active, rel, nil)
		if d.Cat == "" {
			t.Errorf("%s: empty category", rel)
		}
	}
	if d := Decide(active, ".thumbnails/t.jpg", nil); d.Suggest != "thumbnails" || d.Cat != Photos {
		t.Errorf("thumbnail decision = %+v", d)
	}
}

func TestSuggestionIDs(t *testing.T) {
	want := map[string]bool{"thumbnails": true, "system-folders": true, "mac-metadata": true,
		"camera-cache": true, "android-app-data": false, "chkdsk-fragments": false}
	if len(Suggestions) != len(want) {
		t.Fatalf("got %d suggestions", len(Suggestions))
	}
	for id, rec := range want {
		s, ok := Suggestions[id]
		if !ok || s.Recommended != rec || s.Title == "" || s.Why == "" {
			t.Errorf("suggestion %s = %+v", id, s)
		}
	}
	cases := map[string]string{
		"DCIM/.thumbnails/x.jpg": "thumbnails", "System Volume Information/IndexerVolumeGuid": "system-folders",
		"$RECYCLE.BIN/x": "system-folders", "a/._b.jpg": "mac-metadata", "CANONMSC/M0100.CTG": "camera-cache",
		"MISC/A.CTG": "camera-cache", "Android/obb/com.game/main.obb": "android-app-data",
		"FOUND.002/FILE0001.CHK": "chkdsk-fragments", "DCIM/Camera/a.jpg": "",
		"Android/media/com.whatsapp/x.jpg": "",
	}
	for rel, want := range cases {
		if got := Decide(byName("Generic"), rel, nil).Suggest; got != want {
			t.Errorf("%s: suggest %q want %q", rel, got, want)
		}
	}
}

func TestDecideAndroid(t *testing.T) {
	a := byName("Android", "Generic")
	if d := Decide(a, "LOST.DIR/1111", nil); !d.Sniff || d.Cat != Other {
		t.Errorf("LOST.DIR = %+v", d)
	}
	if d := Decide(a, "FOUND.000/FILE0000.CHK", nil); !d.Sniff {
		t.Errorf("FOUND = %+v", d)
	}
	for _, rel := range []string{"WhatsApp/Databases/msgstore.db.crypt14", "WhatsApp/Backups/wa.db.crypt14",
		"Android/media/com.whatsapp/WhatsApp Business/Databases/x.crypt14"} {
		if d := Decide(a, rel, nil); d.Cat != ChatBackups || d.Profile != "Android" {
			t.Errorf("%s = %+v", rel, d)
		}
	}
	if d := Decide(a, "WhatsApp/Media/x.jpg", nil); d.Cat != Photos {
		t.Errorf("whatsapp media = %+v", d)
	}
}

func TestDecideAVCHDTree(t *testing.T) {
	a := byName("AVCHD", "Generic")
	for _, rel := range []string{"PRIVATE/AVCHD/BDMV/INDEX.BDM", "PRIVATE/AVCHD/BDMV/STREAM/00000.MTS", "PRIVATE/AVCHD/BDMV/CLIPINF/00000.CPI"} {
		if d := Decide(a, rel, nil); d.Cat != Videos {
			t.Errorf("%s = %+v", rel, d)
		}
	}
	x := byName("SonyXAVC", "Generic")
	if d := Decide(x, "PRIVATE/M4ROOT/CLIP/C0001M01.XML", nil); d.Cat != Videos {
		t.Errorf("xavc xml = %+v", d)
	}
}

func TestDecideSidecars(t *testing.T) {
	sib := map[string]Category{"dcim/100gopro/gx010001": Videos, "dcim/100media/dji_0001": Videos, "pics/img_1": Photos}
	a := byName("DCFCamera", "GoPro", "DJI", "Generic")
	cases := map[string]Category{
		"DCIM/100GOPRO/GX010001.LRV": Videos, "DCIM/100GOPRO/GX010001.THM": Videos,
		"DCIM/100MEDIA/DJI_0001.SRT": Videos, "pics/IMG_1.xmp": Photos, "pics/IMG_1.aae": Photos,
		"pics/lonely.xmp": Other,
	}
	for rel, want := range cases {
		if d := Decide(a, rel, sib); d.Cat != want {
			t.Errorf("%s = %+v want %v", rel, d, want)
		}
	}
}

func TestUnsniffableFragmentIsOther(t *testing.T) {
	d := Decide(byName("Generic"), "LOST.DIR/2222", nil)
	if !d.Sniff || d.Cat != Other {
		t.Fatalf("generic LOST.DIR = %+v", d)
	}
	if _, _, ok := Sniff(bytes.Repeat([]byte{1, 2, 3, 4, 5}, 100)); ok {
		t.Fatal("random fragment sniffed")
	}
}
