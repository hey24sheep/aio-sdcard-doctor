package rescue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// Final review C1: a folder the card can't list is in the report (FAILED, with the reason), the
// run is not "clean", and manifest.json says so — nothing on the card disappears silently.
func TestUnlistableFolderReported(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.Card{"DCIM/100GOOD/a.jpg": {Data: testutil.JPEG(2000, "a")}, "DCIM/100BROKE/b.jpg": {Data: testutil.JPEG(2000, "b")}})
	dst := filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.listErr = map[string]error{"DCIM/100BROKE": errors.New("I/O error")}
	r := newRescue(src, dst, f)
	r.Types = nil
	res, err := do(t, r, nil, destfs.New(dst, src))
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("a run that couldn't list a folder reported clean")
	}
	e, ok := entries(t, dst)["DCIM/100BROKE/"]
	if !ok || e.Status != manifest.FAILED || !strings.Contains(e.Error, "could not be listed") {
		t.Fatalf("folder row %+v (present %v)", e, ok)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "Reports", "manifest.json"))
	if !strings.Contains(string(b), "DCIM/100BROKE/") {
		t.Fatal("manifest.json doesn't list the unreadable folder")
	}
}

// Adversarial pass (minor): a folder that couldn't be listed once kept its FAILED row after a
// later run listed and copied it, so Card tools kept saying the rescue was incomplete.
func TestFolderListedLaterDropsItsFailedRow(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.Card{"DCIM/100GOOD/a.jpg": {Data: testutil.JPEG(2000, "a")}, "DCIM/100BROKE/b.jpg": {Data: testutil.JPEG(2000, "b")}})
	dst := filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.listErr = map[string]error{"DCIM/100BROKE": errors.New("I/O error")}
	r := newRescue(src, dst, f)
	r.Types = nil
	do(t, r, nil, destfs.New(dst, src))
	r = newRescue(src, dst, newFake(t, src)) // the folder reads fine this time
	r.Types = nil
	res, err := do(t, r, nil, destfs.New(dst, src))
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entries(t, dst)["DCIM/100BROKE/"]; ok {
		t.Fatalf("stale folder row kept: %+v", e)
	}
	if e := entries(t, dst)["DCIM/100BROKE/b.jpg"]; e.Status != manifest.OK || !res.Clean {
		t.Fatalf("file %+v clean %v", e, res.Clean)
	}
}
