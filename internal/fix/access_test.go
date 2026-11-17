package fix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// Adversarial round 2 OA3: every file walked was charged to the budget, so a big photo folder
// sorted before the rescue used it up and the walk answered "no rescue here".
func TestHasRescueNotHiddenByABigFolder(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Pictures"), 0o755)
	for i := 0; i < 20001; i++ {
		os.WriteFile(filepath.Join(root, "Pictures", fmt.Sprintf("IMG_%05d.jpg", i)), nil, 0o644)
	}
	os.MkdirAll(filepath.Join(root, "rescue-card", "Reports"), 0o755)
	os.WriteFile(filepath.Join(root, "rescue-card", "Reports", "state.db"), []byte("x"), 0o644)
	if found, _ := HasRescue(root); !found {
		t.Fatal("a rescue next to a big folder was not seen")
	}
}

func TestHasRescueFindsDeeperRescues(t *testing.T) {
	deep := t.TempDir()
	p := filepath.Join(deep, "Backups", "2026", "cards", "E-card", "Reports")
	os.MkdirAll(p, 0o755)
	os.WriteFile(filepath.Join(p, "manifest.json"), []byte("{}"), 0o644) // state.db gone, manifest kept
	if found, _ := HasRescue(deep); !found {
		t.Fatal("a rescue four folders down was not seen")
	}
}

// When the walk can't look everywhere it must not answer "no rescue": it says it didn't finish.
func TestHasRescueOutOfBudgetIsNotNo(t *testing.T) {
	old := rescueWalkFolders
	defer func() { rescueWalkFolders = old }()
	rescueWalkFolders = 3
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c", "d", "e"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	if found, complete := HasRescue(root); found || complete {
		t.Fatalf("found %v complete %v: an unfinished walk counted as a full answer", found, complete)
	}
	if found, complete := HasRescue(t.TempDir()); found || !complete {
		t.Fatalf("empty disk: found %v complete %v", found, complete)
	}
}

func TestGateWarnsWhenRescueCheckCannotFinish(t *testing.T) {
	warns, err := Gate(card(64<<30), GateInput{Op: OpFormat, Verified: true, Admin: true,
		HasRescue: func(string) (bool, bool) { return false, false }})
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "rescue") {
		t.Fatalf("warns %q err %v", warns, err)
	}
}

// OA5: a resumed or replugged health test skipped its "is this the same card?" check when the
// first block was bad (often block 0 on a worn card), and judged a stranger.
func TestHealthReplugRechecksPastBadFirstBlock(t *testing.T) {
	const size = 64 << 20
	orig := newMem(size, 0)
	orig.wbadFrom, orig.wbadTo = 0, 1<<20 // the first MiB won't take writes
	other := newMem(size, 0)              // a healthy, blank card put in at the replug
	_, err := HealthTest(context.Background(), orig, size, henv(), HealthOptions{
		StorePath: filepath.Join(t.TempDir(), "h.db"), Replug: true,
		Reopen: func() (RW, error) { return other, nil },
	})
	if !errors.Is(err, ErrDifferentCard) {
		t.Fatalf("err %v, want ErrDifferentCard", err)
	}
}

// SA-8: waiting for a replugged card ignored Stop for up to 30 s.
func TestFindAgainStopsOnCancel(t *testing.T) {
	f := &Fix{Target: card(16 << 30), ListDevices: func() ([]drives.Device, error) { return nil, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := f.findAgain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop took %v", d)
	}
}

// SA-7: diskutil verifyVolume exits non-zero when it finds problems; that is a finished check.
func TestDiskutilVerifyProblemsAreAFinishedCheck(t *testing.T) {
	res, err := runWith(t, "darwin", OpCheck, "exfat", exitErr(1))
	if err != nil || res.Counters["PROBLEMS-FOUND"] != 1 || res.Clean {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := interpret("diskutil", OpRepair, 1); got != "failed" {
		t.Fatalf("repair exit 1 = %q", got)
	}
}

// OA8: a health test erases the whole disk, so it must wait for any window reading any of its
// partitions (imaging locks the first one, a rescue of partition 2 locks that one).
func TestHealthTestLocksEveryVolumeOfTheDisk(t *testing.T) {
	n := time.Now().UnixNano() % 1000000
	tg := card(16 << 30)
	tg.Vol.ID = drives.Identity{FSSerial: fmt.Sprintf("BB%06d", n)}
	first := drives.Drive{Root: `P:\`, ID: drives.Identity{FSSerial: fmt.Sprintf("AA%06d", n)}, PhysicalID: "MOCK7-fix"}
	tg.Disk.Volumes = []drives.Drive{first, tg.Vol}
	unlock, err := job.LockSource(first.ID.LockKey()) // another window is imaging partition 1
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := OpenRaw
	defer func() { OpenRaw = old }()
	opened := false
	OpenRaw = func(Target) (RW, io.Closer, error) { opened = true; return nil, nil, errors.New("not here") }
	f := &Fix{Op: OpHealth, Target: tg, GOOS: "windows", Admin: func() bool { return true }, StoreDir: t.TempDir()}
	if _, err := f.Run(context.Background(), henv(), job.Plan{}); !errors.Is(err, job.ErrSourceBusy) || opened {
		t.Fatalf("health test of partition 2 ran while partition 1 was in use: err %v opened %v", err, opened)
	}
}

// SA-6: a read-only check may be stopped; a repair or format is left to finish.
func TestOnlyACheckIsKilledOnStop(t *testing.T) {
	cases := map[string]bool{
		"chkdsk Q:":                          true,
		"chkdsk Q: /f /x":                    false,
		"format Q: /FS:exFAT /Q /Y":          false,
		"diskutil verifyVolume /dev/disk4s1": true,
		"fsck.vfat -n /dev/sdb1":             true,
		"fsck.vfat -a /dev/sdb1":             false,
	}
	for line, want := range cases {
		if got := readOnlyTool(strings.Fields(line)); got != want {
			t.Errorf("%s: read-only %v, want %v", line, got, want)
		}
	}
}
