package rescue

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// #6: switching compress on a re-run never reads the card again for files already stored.
func TestCompressSwitchDoesNotReread(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	f := newFake(t, src)
	res, err := do(t, newRescue(src, dst, f), nil, nil) // plain folder this time
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["ALREADY-COPIED"] != len(card) {
		t.Fatalf("card re-read after switching output format: %v", res.Counters)
	}
}

// #6: the choice is remembered for the next run.
func TestChoiceRemembered(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	p, _ := profile.PresetByID("quick")
	s := SelectionFor(p)
	s.SkipEmpty = true
	r.Backup, r.Types, r.Exclude, r.SkipEmpty, r.Compress = s.Name, s.Types, s.Exclude, true, true
	do(t, r, nil, nil)
	prev, compress, ok := PreviousChoice(dst)
	if !ok || prev.Name != "Quick" || !prev.SkipEmpty || !compress || len(prev.Types) != 3 {
		t.Fatalf("%+v %v %v", prev, compress, ok)
	}
}

// #7: a narrower re-run keeps earlier rows, counts them, and a kept FAILED row is not "clean".
func TestNarrowerRerunReportsKept(t *testing.T) {
	card := testutil.AndroidCard(false)
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.openErr["Music/song.mp3"] = []error{errors.New("i/o error")}
	do(t, newRescue(src, dst, f), nil, nil) // Full; the song FAILED
	r := newRescue(src, dst, newFake(t, src))
	r.Types, r.Backup = []profile.Category{profile.Photos}, "Photos only"
	res, err := do(t, r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["KEPT-FROM-EARLIER-RUN"] == 0 || res.Clean {
		t.Fatalf("%v clean=%v", res.Counters, res.Clean)
	}
	if e := entries(t, dst)["Music/song.mp3"]; e.Status != manifest.FAILED {
		t.Fatalf("earlier row changed: %+v", e)
	}
}
