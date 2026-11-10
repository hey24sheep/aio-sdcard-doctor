package rescue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// C2: reading a card directly must never write onto that same card.
func TestRawDeviceDestOnSameCardRefused(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.From, r.Raw = `\\.\PhysicalDrive7`, true
	r.LookupDrive = func(p string) (drives.Drive, error) {
		if p == r.From {
			return drives.Drive{}, errors.New("no volume found") // like drives.Lookup on a device path
		}
		return drives.Drive{Root: `E:\`, FS: "exFAT", PhysicalID: "PhysicalDrive7", Free: 1 << 40}, nil
	}
	blocked(t, r, "same-disk")
	if _, err := os.Stat(filepath.Join(dst, "Reports")); err == nil {
		t.Fatal("probed (wrote) onto the card before refusing")
	}
}

// I1: when retries run out the file is FAILED, never a blank status.
func TestRetriesExhaustedIsFailed(t *testing.T) {
	card := testutil.Card{"a.jpg": {Data: testutil.JPEG(5000, "a")}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.opt.OnRead = func(int64, int) error { return errors.New("i/o error") }
	r := newRescue(src, dst, f)
	n := 0
	r.Present = func(drives.Identity, string) (string, bool) { n++; return src, n%2 == 0 } // keeps coming and going
	res, err := do(t, r, &prompter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := entries(t, dst)["a.jpg"]; e.Status != manifest.FAILED || res.Clean {
		t.Fatalf("%+v clean=%v", e, res.Clean)
	}
}

// I2: a damaged state.db is rebuilt from manifest.json instead of re-reading the card.
func TestCorruptStoreRebuiltNotReread(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	os.WriteFile(filepath.Join(dst, "Reports", "state.db"), make([]byte, 64<<10), 0o644)
	f := newFake(t, src)
	res, err := do(t, newRescue(src, dst, f), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["ALREADY-COPIED"] != len(card) {
		t.Fatalf("not rebuilt: %v", res.Counters)
	}
}

// I3: rescuing from a card image needs no admin rights.
func TestRescueFromImageNeedsNoAdmin(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	img := filepath.Join(t.TempDir(), "card.img")
	os.WriteFile(img, []byte("x"), 0o644)
	r := newRescue(src, dst, newFake(t, src))
	r.From, r.Raw = img, true
	p, err := r.Plan(context.Background(), job.Env{})
	defer r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if p.NeedsAdmin {
		t.Fatal("image file treated as needing admin")
	}
	for _, is := range p.Issues {
		if is.Kind == "needs-admin" {
			t.Fatal("needs-admin issue for an image file")
		}
	}
}

// I5: a card read directly comes back under another drive letter.
func TestPresentRawVolumeComesBackAsOtherLetter(t *testing.T) {
	old := drives.Lister
	defer func() { drives.Lister = old }()
	drives.Lister = func() ([]drives.Drive, error) {
		return []drives.Drive{{Root: `F:\`, ID: drives.Identity{FSSerial: "S1", Size: 64}}}, nil
	}
	if got, ok := present(drives.Identity{FSSerial: "S1", Size: 64}, `\\.\E:`); !ok || got != `\\.\F:` {
		t.Fatalf("%q %v", got, ok)
	}
}

// I5/C1: a whole-device source is found again only if it is the same card, never a same-size
// stranger.
func TestPresentRawDevicePrefersSamePathNeverStranger(t *testing.T) {
	old := drives.DeviceLister
	defer func() { drives.DeviceLister = old }()
	drives.DeviceLister = func() ([]drives.Device, error) {
		return []drives.Device{{Path: `\\.\PhysicalDrive2`, Size: 64}, {Path: `\\.\PhysicalDrive3`, Size: 64}}, nil
	}
	id := drives.Identity{Size: 64, FSSerial: "S1"}
	if got, ok := present(id, `\\.\PhysicalDrive3`); !ok || got != `\\.\PhysicalDrive3` {
		t.Fatalf("%q %v", got, ok)
	}
	if got, ok := present(id, `\\.\PhysicalDrive9`); ok {
		t.Fatalf("switched to a stranger of the same size: %q", got)
	}
}

// I6: two folders of one card are one card.
func TestTwoFoldersOfOneCardShareLock(t *testing.T) {
	id := drives.Identity{FSSerial: "S1", Size: 64}
	if lockKey(id, `E:\DCIM`, true) != lockKey(id, `E:\Android`, true) {
		t.Fatal("two folders of one card get different locks")
	}
	if lockKey(id, `C:\a`, false) == lockKey(id, `C:\b`, false) {
		t.Fatal("folders on a fixed disk (not cards) should not block each other")
	}
}

// I8: a sector that keeps stalling is zero-filled like a bad sector; the file is PARTIAL.
func TestStallInsideFileZeroFills(t *testing.T) {
	card := testutil.Card{"a.jpg": {Data: testutil.JPEG(200_000, "a")}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.opt.OnRead = func(off int64, n int) error {
		if off < 100_000 && off+int64(n) > 90_000 {
			return source.ErrStalled
		}
		return nil
	}
	if _, err := do(t, newRescue(src, dst, f), &prompter{}, nil); err != nil {
		t.Fatal(err)
	}
	if e := entries(t, dst)["a.jpg"]; e.Status != manifest.PARTIAL || e.BadBytes == 0 {
		t.Fatalf("%+v", e)
	}
}

// I14: a daylight-saving change shifts FAT times by exactly one hour.
func TestSameTimeAcrossDST(t *testing.T) {
	a := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if !sameTime(a, a.Add(time.Hour)) || !sameTime(a, a.Add(-time.Hour+time.Second)) || sameTime(a, a.Add(30*time.Minute)) {
		t.Fatal("DST shift handling wrong")
	}
}

var _ = faultreader.Options{}

// Choosing "stop" ends with a plain message, not "context canceled".
func TestStopByUserMessage(t *testing.T) {
	card := testutil.Card{"a.jpg": {Data: testutil.JPEG(200_000, "a")}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	clk := &fakeClock{t: time.Unix(0, 0)}
	f.opt = faultreader.Options{FailDelay: 25 * time.Second, Sleep: clk.sleep}
	f.bad["a.jpg"] = []faultreader.Range{{Off: 50_000, Len: 100_000}}
	r := newRescue(src, dst, f)
	r.Now = clk.now
	_, err := do(t, r, &prompter{answers: map[string]job.Choice{"file-slow": job.Stop}}, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped by you") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v", err)
	}
}
