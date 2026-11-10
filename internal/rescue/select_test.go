package rescue

import (
	"context"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

func androidInventory(t *testing.T) scan.Inventory {
	t.Helper()
	src, err := source.OpenFolder(testutil.WriteFolder(t, testutil.AndroidCard(false)))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := scan.Scan(context.Background(), src, profile.Detect(src))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func picked(t *testing.T, id string) (map[string]bool, map[string]string) {
	t.Helper()
	p, ok := profile.PresetByID(id)
	if !ok {
		t.Fatalf("no preset %s", id)
	}
	sel, ex := Select(androidInventory(t), SelectionFor(p))
	in := map[string]bool{}
	for _, it := range sel {
		in[it.Rel] = true
	}
	return in, ex
}

func TestPresetQuick(t *testing.T) {
	in, ex := picked(t, "quick")
	for _, rel := range []string{"DCIM/Camera/IMG_20240101_101010.jpg", "Docs/report.pdf", "WhatsApp/Databases/msgstore.db.crypt14", "Telegram/Telegram Images/tg1.jpg"} {
		if !in[rel] {
			t.Errorf("quick lacks %s (%s)", rel, ex[rel])
		}
	}
	for _, rel := range []string{"DCIM/Camera/VID_20240103.mp4", "Music/song.mp3", "Docs/bundle.zip", "notes.ini", "DCIM/.thumbnails/t1.jpg"} {
		if in[rel] || ex[rel] == "" {
			t.Errorf("quick includes %s / no reason", rel)
		}
	}
}

func TestPresetStandardSkipsJunkAndOther(t *testing.T) {
	in, ex := picked(t, "standard")
	if !in["DCIM/Camera/VID_20240103.mp4"] || !in["Music/song.mp3"] || !in["Docs/bundle.zip"] {
		t.Fatal("standard lacks media")
	}
	if in["notes.ini"] || in["DCIM/.thumbnails/t1.jpg"] || ex["DCIM/.thumbnails/t1.jpg"] != "thumbnails" {
		t.Fatalf("standard took junk: thumbs reason %q", ex["DCIM/.thumbnails/t1.jpg"])
	}
}

func TestPresetFullTakesEverything(t *testing.T) {
	in, ex := picked(t, "full")
	if len(ex) != 0 || len(in) != len(testutil.AndroidCard(false)) {
		t.Fatalf("full excluded %v", ex)
	}
}

func TestPresetSingleTypes(t *testing.T) {
	for id, want := range map[string]profile.Category{"photos": profile.Photos, "videos": profile.Videos, "music": profile.Music, "documents": profile.Documents} {
		in, _ := picked(t, id)
		if len(in) == 0 {
			t.Fatalf("%s: nothing", id)
		}
		for rel := range in {
			if c := testutil.AndroidCard(false)[rel].Cat; c != want {
				t.Errorf("%s includes %s (%s)", id, rel, c)
			}
		}
	}
}

func TestPresetWhatsAppOnly(t *testing.T) {
	in, ex := picked(t, "whatsapp")
	want := []string{"WhatsApp/Databases/msgstore.db.crypt14", "WhatsApp/Backups/wa.db.crypt14",
		"Android/media/com.whatsapp/WhatsApp/Media/WhatsApp Images/IMG-1.jpg", "WhatsApp/Media/WhatsApp Voice Notes/PTT-1.opus"}
	for _, rel := range want {
		if !in[rel] {
			t.Errorf("whatsapp lacks %s", rel)
		}
	}
	if len(in) != len(want) {
		t.Errorf("whatsapp took %d files: %v", len(in), in)
	}
	if !strings.Contains(ex["DCIM/Camera/IMG_20240101_101010.jpg"], "WhatsApp") {
		t.Errorf("reason %q", ex["DCIM/Camera/IMG_20240101_101010.jpg"])
	}
}

func TestPresetTelegramOnly(t *testing.T) {
	in, _ := picked(t, "telegram")
	if len(in) != 2 || !in["Telegram/Telegram Images/tg1.jpg"] || !in["Android/media/org.telegram.messenger/Telegram/Telegram Video/tgv.mp4"] {
		t.Fatalf("telegram took %v", in)
	}
}

func TestSkipEmptyFiles(t *testing.T) {
	p, _ := profile.PresetByID("full")
	s := SelectionFor(p)
	inv := androidInventory(t)
	sel, _ := Select(inv, s)
	took := map[string]bool{}
	for _, it := range sel {
		took[it.Rel] = true
	}
	if !took["Download/empty.pdf"] {
		t.Fatal("empty file dropped without being asked")
	}
	s.SkipEmpty = true
	sel, ex := Select(inv, s)
	for _, it := range sel {
		if it.Size == 0 {
			t.Fatalf("empty file %s kept", it.Rel)
		}
	}
	if !strings.Contains(ex["Download/empty.pdf"], "0 bytes") {
		t.Fatalf("reason %q", ex["Download/empty.pdf"])
	}
}
