package scan

import (
	"context"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// #8: a photo or video with an unknown extension is recognised by its content, so presets
// that leave out "Other" don't drop it; its name is kept.
func TestUnknownExtensionSniffed(t *testing.T) {
	card := testutil.Card{
		"DCIM/IMG_0001.xyz": {Data: testutil.JPEG(3000, "x")},
		"DCIM/clip.vid":     {Data: testutil.MP4(5000, "v")},
		"settings.dat":      {Data: []byte("plain settings text, not media at all......")},
	}
	src, _ := source.OpenFolder(testutil.WriteFolder(t, card))
	inv, err := Scan(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Item{}
	for _, it := range inv.Items {
		got[it.Rel] = it
	}
	if it := got["DCIM/IMG_0001.xyz"]; it.Cat != profile.Photos || it.Ext != ".xyz" || it.Fragment {
		t.Errorf("jpeg with unknown extension: %+v", it)
	}
	if it := got["DCIM/clip.vid"]; it.Cat != profile.Videos {
		t.Errorf("mp4 with unknown extension: %+v", it)
	}
	if it := got["settings.dat"]; it.Cat != profile.Other {
		t.Errorf("settings: %+v", it)
	}
}
