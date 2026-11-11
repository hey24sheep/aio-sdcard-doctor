package fix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// --- M3 review #1: Linux/macOS-shaped targets --------------------------------------------------

func TestGateRefusesSystemTargetVolume(t *testing.T) {
	c := card(64 << 30)
	c.Vol.System = true // the named volume is the system, even if the disk's list lacks the flag
	if _, err := gate(c, GateInput{Verified: true, Admin: true, AllowFixed: true}); !refused(err, "system") {
		t.Fatalf("err %v", err)
	}
}

func TestGateRefusesDiskInUseByOS(t *testing.T) {
	c := card(500 << 30)
	c.Disk.InUse = "part of an LVM, RAID or encrypted volume"
	if _, err := gate(c, GateInput{Verified: true, Admin: true, AllowFixed: true}); !refused(err, "LVM") {
		t.Fatalf("err %v", err)
	}
}

func TestLinuxUnmountedUnknownFS(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(name string) (string, error) { return "/sbin/" + name, nil }
	c := card(16 << 30)
	c.Vol.Root, c.Vol.FS = "", "" // unmounted: Linux doesn't know its file system without probing
	if cs := Available("linux", c, ""); !cs[0].Enabled || !cs[1].Enabled {
		t.Fatalf("caps %+v", cs)
	}
	for op, want := range map[string][]string{OpCheck: {"fsck", "-n", "/dev/mock1"}, OpRepair: {"fsck", "-a", "/dev/mock1"}} {
		if got, err := Command("linux", op, c, ""); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v %v", op, got, err)
		}
	}
	if cs := Available("windows", c, ""); cs[0].Enabled {
		t.Fatal("windows: unknown file system checkable")
	}
}

// --- M3 review #3: tools' exit codes ------------------------------------------------------------

type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func runWith(t *testing.T, goos, op, fs string, err error) (job.Result, error) {
	t.Helper()
	old := Exec
	defer func() { Exec = old }()
	Exec = func(context.Context, []string, io.Writer) error { return err }
	c := card(16 << 30)
	c.Vol.FS = fs
	if goos == "linux" && op != OpCheck {
		c.Vol.Root = ""
	}
	f := &Fix{Op: op, Target: c, GOOS: goos, Admin: func() bool { return true }}
	return f.Run(context.Background(), henv(), job.Plan{})
}

func TestRepairExitCodes(t *testing.T) {
	cases := []struct {
		goos, op, fs string
		err          error
		fails, clean bool
	}{
		{"windows", OpRepair, "fat32", nil, false, true},
		{"windows", OpRepair, "fat32", exitErr(1), false, true}, // errors found and fixed
		{"windows", OpRepair, "fat32", exitErr(3), true, false}, // couldn't fix
		{"windows", OpCheck, "fat32", exitErr(3), false, false}, // problems found
		{"windows", OpCheck, "fat32", nil, false, true},
		// linux repair exit 1 ("corrected") now depends on a re-check: TestLinuxRepairFixedOnlyIfRecheckClean
		{"linux", OpRepair, "fat32", exitErr(4), true, false}, // fsck: left uncorrected
		{"linux", OpCheck, "fat32", exitErr(4), false, false}, // fsck -n: errors found
		{"linux", OpCheck, "fat32", exitErr(8), true, false},  // fsck: operational error
		{"windows", OpCheck, "fat32", errors.New(`exec: "chkdsk": executable file not found`), true, false},
		{"darwin", OpFormat, "fat32", exitErr(1), true, false},
	}
	for _, c := range cases {
		res, err := runWith(t, c.goos, c.op, c.fs, c.err)
		if (err != nil) != c.fails || err == nil && res.Clean != c.clean {
			t.Errorf("%s %s %v: res %+v err %v", c.goos, c.op, c.err, res, err)
		}
	}
}

// --- M3 review #4..#8: health test ---------------------------------------------------------------

func TestHealthNewTestAfterOtherCard(t *testing.T) {
	old := healthCheckpoint
	healthCheckpoint = 2 << 20
	defer func() { healthCheckpoint = old }()
	a := newMem(16<<20, 0)
	a.failAfter = 9
	o := hopts(t)
	HealthTest(context.Background(), a, a.size, henv(), o)
	b := newMem(16<<20, 0)
	if _, err := HealthTest(context.Background(), b, b.size, henv(), o); !errors.Is(err, ErrDifferentCard) || !strings.Contains(err.Error(), "--new-test") {
		t.Fatalf("err %v", err)
	}
	o.Restart = true
	if rep, err := HealthTest(context.Background(), b, b.size, henv(), o); err != nil || len(rep.Bad) != 0 {
		t.Fatalf("--new-test: %+v %v", rep, err)
	}
}

func TestHealthRealCapacityCountsOnce(t *testing.T) {
	c := newMem(16<<20, 0)
	c.wbadFrom, c.wbadTo = 3<<20, 4<<20 // writes fail here (and it reads back wrong)
	rep, err := HealthTest(context.Background(), c, c.size, henv(), hopts(t))
	if err != nil || rep.RealCapacity != 15<<20 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestHealthCardPulledWhileReading(t *testing.T) {
	old := healthCheckpoint
	healthCheckpoint = 4 << 20
	defer func() { healthCheckpoint = old }()
	c := newMem(16<<20, 0)
	c.pulledFrom = 2 << 20
	o := hopts(t)
	if _, err := HealthTest(context.Background(), c, c.size, henv(), o); err == nil {
		t.Fatal("no error when the card went away")
	}
	c.pulledFrom = 0 // back in the reader
	rep, err := HealthTest(context.Background(), c, c.size, henv(), o)
	if err != nil || len(rep.Bad) != 0 || rep.RealCapacity != c.size {
		t.Fatalf("healthy card after a pull: %+v %v", rep, err)
	}
}

func TestHealthDropWriteFake(t *testing.T) {
	c := newMem(32<<20, 0)
	c.dropFrom = 12 << 20 // says 32 MB; writes past 12 MB vanish, reads there give zeros
	rep, err := HealthTest(context.Background(), c, c.size, henv(), hopts(t))
	if err != nil || !rep.Fake || rep.RealCapacity != 12<<20 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestHealthReopenFailureNoPanic(t *testing.T) {
	defer func(w time.Duration) { findAgainWait = w }(findAgainWait)
	findAgainWait = 0 // the card never comes back here: don't wait for it
	c := newMem(4<<20, 0)
	oldOpen := OpenRaw
	defer func() { OpenRaw = oldOpen }()
	opens := 0
	OpenRaw = func(Target) (RW, io.Closer, error) {
		if opens++; opens > 1 {
			return nil, nil, errors.New("not back yet")
		}
		return c, io.NopCloser(nil), nil
	}
	tg := card(4 << 20)
	f := &Fix{Op: OpHealth, Target: tg, Replug: true, StoreDir: t.TempDir(), GOOS: "windows", Admin: func() bool { return true },
		ListDevices: func() ([]drives.Device, error) { return []drives.Device{tg.Disk}, nil }}
	if _, err := f.Run(context.Background(), henv(), job.Plan{}); err == nil {
		t.Fatal("reopen failure not reported")
	}
}

// Adversarial pass: a dead area of 4 MiB or more on a card that is still plugged in (a failing
// card, or a fake that rejects writes past its real size) used to stop the test as "card pulled"
// at the same place on every run — no verdict, ever. It is a bad area: the test goes on.
func TestHealthLongDeadAreaStillGivesVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *memCard)
	}{
		{"writes", func(c *memCard) { c.wbadFrom, c.wbadTo = 20<<20, 28<<20 }},
		{"reads", func(c *memCard) { c.badFrom, c.badTo = 20<<20, 28<<20 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMem(64<<20, 0)
			tc.set(c)
			rep, err := HealthTest(context.Background(), c, c.size, henv(), hopts(t))
			if err != nil {
				t.Fatalf("no verdict: %v", err)
			}
			var bad int64
			for _, r := range rep.Bad {
				bad += r.N
			}
			if bad < 8<<20 || rep.RealCapacity > 56<<20 {
				t.Fatalf("dead area not reported: %+v", rep)
			}
		})
	}
}

// Adversarial pass: fsck.fat exits 1 both for "errors corrected" and for a fatal error (an
// unreadable boot sector), and 2 for a usage error that never touched the card. A Linux repair
// is "fixed" only when a check-only pass afterwards comes back clean.
func TestLinuxRepairFixedOnlyIfRecheckClean(t *testing.T) {
	cases := []struct {
		codes []int // repair, then the re-check
		fails bool
		ran   int
	}{
		{[]int{1, 0}, false, 2}, // corrected, re-check clean → fixed
		{[]int{1, 1}, true, 2},  // "corrected" but re-check still finds problems (or the card is unreadable)
		{[]int{2, 0}, true, 1},  // usage error / didn't touch the file system
		{[]int{0}, false, 1},    // nothing to fix
	}
	for _, c := range cases {
		old := Exec
		var ran [][]string
		Exec = func(_ context.Context, argv []string, _ io.Writer) error {
			code := c.codes[min(len(ran), len(c.codes)-1)]
			ran = append(ran, argv)
			if code == 0 {
				return nil
			}
			return exitErr(code)
		}
		tg := card(16 << 30)
		tg.Vol.FS, tg.Vol.Root = "fat32", ""
		f := &Fix{Op: OpRepair, Target: tg, GOOS: "linux", Admin: func() bool { return true }}
		res, err := f.Run(context.Background(), henv(), job.Plan{})
		Exec = old
		if (err != nil) != c.fails || len(ran) != c.ran {
			t.Errorf("codes %v: res %+v err %v ran %v", c.codes, res, err, ran)
		}
		if len(ran) == 2 && !slices.Contains(ran[1], "-n") {
			t.Errorf("re-check wasn't check-only: %v", ran[1])
		}
	}
}

// Adversarial pass S3: card tools never took the per-card lock, so a health test could write
// its patterns over a card another window was imaging (they ended up in card.img).
func TestCardToolsTakeTheCardLock(t *testing.T) {
	tg := card(16 << 30)
	tg.Vol.ID = drives.Identity{FSSerial: fmt.Sprintf("LOCK-%d", time.Now().UnixNano()%100000)}
	unlock, err := job.LockSource(tg.Vol.ID.LockKey()) // another window is reading this card
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := Exec
	defer func() { Exec = old }()
	ran := false
	Exec = func(context.Context, []string, io.Writer) error { ran = true; return nil }
	f := &Fix{Op: OpRepair, Target: tg, GOOS: "windows", Admin: func() bool { return true }}
	if _, err := f.Run(context.Background(), henv(), job.Plan{}); !errors.Is(err, job.ErrSourceBusy) || ran {
		t.Fatalf("repair ran on a card in use: err %v ran %v", err, ran)
	}
}

// Adversarial pass A5: "refuses the disk holding a rescue" missed card images (card.img +
// card.img.json/.map) and rescues two folders down (Backups/<rescue>/Reports/state.db).
func TestHasRescueFindsImagesAndDeeperRescues(t *testing.T) {
	img := t.TempDir()
	os.WriteFile(filepath.Join(img, "card.img.json"), []byte("{}"), 0o644)
	deep := t.TempDir()
	rep := filepath.Join(deep, "Backups", "aio-sdcard-doctor-CAMERA-20261110", "Reports")
	os.MkdirAll(rep, 0o755)
	os.WriteFile(filepath.Join(rep, "state.db"), nil, 0o644)
	for name, root := range map[string]string{"image": img, "two folders down": deep} {
		if !HasRescue(root) {
			t.Errorf("%s: not seen as holding a rescue", name)
		}
	}
	if HasRescue(t.TempDir()) {
		t.Error("an empty disk holds a rescue")
	}
}

// Adversarial pass A3: after a health test the card has no partition, so this tool can't format
// it on any OS — yet Format showed as available and the plan then refused it.
func TestFormatGreyedWhenThereIsNoVolume(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(name string) (string, error) { return "/sbin/" + name, nil }
	blank := card(16 << 30)
	blank.Vol = drives.Drive{} // what the health test leaves: no partition, no volume
	for _, goos := range []string{"windows", "darwin", "linux"} {
		if c := Available(goos, blank, "")[2]; c.Enabled || c.Reason == "" {
			t.Errorf("%s: Format %+v", goos, c)
		}
	}
}

// Adversarial pass A4: once the test has written over the partition table the drive letter (or
// /dev/sdb1) is gone, so "run the same command again" failed. The message names the card's disk.
func TestHealthStopNamesTheDiskToContinueWith(t *testing.T) {
	c := newMem(16<<20, 0)
	c.failAfter = 6 // pulled while writing
	oldOpen := OpenRaw
	defer func() { OpenRaw = oldOpen }()
	OpenRaw = func(Target) (RW, io.Closer, error) { return c, io.NopCloser(nil), nil }
	tg := card(16 << 20)
	f := &Fix{Op: OpHealth, Target: tg, StoreDir: t.TempDir(), GOOS: "windows", Admin: func() bool { return true },
		ListDevices: func() ([]drives.Device, error) { return []drives.Device{tg.Disk}, nil }}
	_, err := f.Run(context.Background(), henv(), job.Plan{})
	if err == nil || !strings.Contains(err.Error(), "--drive "+tg.Disk.Path) {
		t.Fatalf("stop message doesn't say how to continue: %v", err)
	}
}

// Adversarial pass A6: Stop killed chkdsk/fsck/format mid-run — the worst moment for the card's
// file system. The tool is left to finish (Windows) or interrupted (elsewhere), never killed.
func TestStopDoesNotKillTheTool(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows: the tool is left to finish; elsewhere it gets SIGINT")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	Exec(ctx, []string{"cmd", "/c", "ping", "-n", "3", "127.0.0.1"}, io.Discard) // ~2 s, harmless
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Fatalf("the tool was killed after %v", d)
	}
}
