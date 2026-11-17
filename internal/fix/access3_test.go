package fix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// uniqueCard is a card on a physical disk no other test (or test binary) uses, so the card locks
// taken here never meet another run's.
func uniqueCard(size uint64) Target {
	t := card(size)
	id := fmt.Sprintf("MOCKDISK%d", time.Now().UnixNano())
	t.Vol.PhysicalID, t.Disk.PhysicalID, t.Vol.ID.PhysicalID = id, id, id
	return t
}

func capsByName(cs []job.Capability) map[string]job.Capability {
	out := map[string]job.Capability{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

// OA5/SA-3: on Linux a mounted card was offered the health test (and Plan passed); only once the
// user had typed the label and size did it say "unmount the card first" — and once unmounted the
// window no longer lists the card. Repair, format and the health test are greyed out with exactly
// what to do.
func TestLinuxMountedCardSaysHowToUnmount(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(name string) (string, error) { return "/sbin/" + name, nil }
	tg := card(16 << 30)
	tg.Vol.Root, tg.Vol.Device, tg.Vol.FS = "/media/u/CAMERA", "/dev/sdb1", "vfat"
	tg.Disk.Path, tg.Disk.PhysicalID = "/dev/sdb", "sdb"
	tg.Disk.Volumes = []drives.Drive{tg.Vol, {Device: "/dev/sdb2", Root: "/media/u/OTHER"}}
	cs := capsByName(Available("linux", tg, ""))
	if !cs["Check"].Enabled {
		t.Errorf("a check changes nothing, mounted or not: %+v", cs["Check"])
	}
	for name, want := range map[string][]string{
		"Repair":      {"sudo umount /dev/sdb1", "fix --drive /dev/sdb1 --repair"},
		"Format":      {"sudo umount /dev/sdb1", "fix --drive /dev/sdb1 --format"},
		"Health test": {"sudo umount /dev/sdb1 /dev/sdb2", "fix --drive /dev/sdb --health-test"},
	} {
		c := cs[name]
		for _, w := range want {
			if c.Enabled || !strings.Contains(c.Reason, w) {
				t.Errorf("%s: %+v, want %q", name, c, w)
			}
		}
	}
	f := &Fix{Op: OpHealth, Target: tg, GOOS: "linux", Admin: func() bool { return true }}
	if p, _ := f.Plan(context.Background(), henv()); len(p.Issues) == 0 || p.Issues[0].Kind != "not-available" {
		t.Fatalf("plan of a health test of a mounted card: %+v", p.Issues)
	}
	// unmounted, everything is offered
	tg.Vol.Root, tg.Disk.Volumes[1].Root = "", ""
	tg.Disk.Volumes[0] = tg.Vol
	for name, c := range capsByName(Available("linux", tg, "")) {
		if !c.Enabled {
			t.Errorf("unmounted: %s greyed: %s", name, c.Reason)
		}
	}
}

// SA-10: when the disk's size couldn't be read the health test fell back to the volume's size,
// tested the volume only and reported on "the whole card".
func TestHealthTestNeedsTheDiskSize(t *testing.T) {
	tg := card(16 << 30)
	tg.Disk.Size = 0
	if c := capsByName(Available("windows", tg, ""))["Health test"]; c.Enabled || !strings.Contains(c.Reason, "size") {
		t.Fatalf("%+v", c)
	}
	f := &Fix{Op: OpHealth, Target: tg, GOOS: "windows", Admin: func() bool { return true }}
	if p, _ := f.Plan(context.Background(), henv()); len(p.Issues) == 0 || p.Issues[0].Kind != "not-available" {
		t.Fatalf("plan: %+v", p.Issues)
	}
}

// SA-8: a disk holding a rescue was refused even for a check, which only reads.
func TestCheckAllowedOnADiskHoldingARescue(t *testing.T) {
	in := GateInput{Op: OpCheck, Admin: true, HasRescue: func(string) (bool, bool) { return true, true }}
	if _, err := Gate(card(16<<30), in); err != nil {
		t.Fatalf("check refused: %v", err)
	}
	in.Op = OpFormat
	if _, err := Gate(card(16<<30), in); err == nil {
		t.Fatal("format allowed on a disk holding a rescue")
	}
}

// OA3: imaging and recover lock a card without a serial by keys of their own (size, physical id
// and content samples), so Format or the health test ran while another window imaged the same
// RAW card. Every job reading or writing a physical disk also takes its disk lock; repair and
// format take every volume's lock too (a rescue of partition 1 while partition 2 is formatted).
func TestCardToolsWaitForAnyJobOnTheDisk(t *testing.T) {
	old := Exec
	defer func() { Exec = old }()
	for _, op := range []string{OpCheck, OpRepair, OpFormat, OpHealth} {
		for name, setup := range map[string]func(*Target) string{
			"disk lock held (imaging a RAW card)": func(tg *Target) string {
				tg.Vol.FS, tg.Vol.RAW = "RAW", true
				return drives.Identity{PhysicalID: tg.Disk.PhysicalID}.DiskLockKey()
			},
			"another partition is being rescued": func(tg *Target) string {
				other := drives.Drive{Root: `P:\`, PhysicalID: tg.Disk.PhysicalID,
					ID: drives.Identity{FSSerial: fmt.Sprintf("%08X", time.Now().UnixNano()&0xFFFFFFF)}}
				tg.Vol.ID.FSSerial = fmt.Sprintf("%08X", (time.Now().UnixNano()+1)&0xFFFFFFF)
				tg.Disk.Volumes = []drives.Drive{other, tg.Vol}
				return other.ID.LockKey()
			},
		} {
			if op == OpCheck && name == "another partition is being rescued" {
				continue // a check of one partition changes nothing anywhere
			}
			tg := uniqueCard(16 << 30)
			key := setup(&tg)
			unlock, err := job.LockSource(key)
			if err != nil {
				t.Fatal(err)
			}
			ran := false
			Exec = func(context.Context, []string, io.Writer) error { ran = true; return nil }
			oldRaw := OpenRaw
			OpenRaw = func(Target) (RW, io.Closer, error) { ran = true; return nil, nil, errors.New("not here") }
			f := &Fix{Op: op, Target: tg, GOOS: "windows", Admin: func() bool { return true }, StoreDir: t.TempDir()}
			_, err = f.Run(context.Background(), henv(), job.Plan{})
			OpenRaw = oldRaw
			unlock()
			if !errors.Is(err, job.ErrSourceBusy) || ran {
				t.Errorf("%s, %s: err %v ran %v", op, name, err, ran)
			}
		}
	}
}

// OA6/SA-4: the walk was depth-first, so one big early folder used up the budget before a rescue
// one level down was seen; and a folder that couldn't be listed counted as looked at.
func TestHasRescueLooksBreadthFirst(t *testing.T) {
	old := rescueWalkFolders
	defer func() { rescueWalkFolders = old }()
	rescueWalkFolders = 50
	root := t.TempDir()
	deep := filepath.Join(root, "a-photos")
	for i := 0; i < 5; i++ { // a deep tree sorted first: 5 levels of 12 folders each
		for j := 0; j < 12; j++ {
			os.MkdirAll(filepath.Join(deep, fmt.Sprintf("d%02d", j)), 0o755)
		}
		deep = filepath.Join(deep, "d00")
	}
	os.MkdirAll(filepath.Join(root, "z-backup", "Reports"), 0o755)
	os.WriteFile(filepath.Join(root, "z-backup", "Reports", "state.db"), []byte("x"), 0o644)
	if found, _ := HasRescue(root); !found {
		t.Fatal("a rescue one folder down was missed behind a deep folder")
	}
}

func TestHasRescueUnlistableFolderIsNotLookedAt(t *testing.T) {
	old := readDir
	defer func() { readDir = old }()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "broken"), 0o755)
	readDir = func(dir string) ([]os.DirEntry, error) {
		if filepath.Base(dir) == "broken" {
			return nil, errors.New("I/O error")
		}
		return os.ReadDir(dir)
	}
	if found, complete := HasRescue(root); found || complete {
		t.Fatalf("found %v complete %v: an unreadable folder counted as looked at", found, complete)
	}
}

// OA7: the walk runs in our process on a dying card: a read that never returns hung Plan, and
// Stop couldn't end it. It gives up after a time limit (or on Stop) with "couldn't look
// everywhere"; and the named volume (also in the disk's list) is walked once.
func TestHasRescueGivesUpOnAStuckCard(t *testing.T) {
	oldRead, oldTime := readDir, rescueWalkTime
	defer func() { readDir, rescueWalkTime = oldRead, oldTime }()
	stuck := make(chan struct{})
	defer close(stuck)
	readDir = func(string) ([]os.DirEntry, error) { <-stuck; return nil, nil }
	rescueWalkTime = 200 * time.Millisecond
	start := time.Now()
	if found, complete := HasRescue(t.TempDir()); found || complete {
		t.Fatalf("found %v complete %v", found, complete)
	}
	rescueWalkTime = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := Gate(card(16<<30), GateInput{Ctx: ctx, Op: OpFormat, Admin: true, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestGateWalksEachVolumeOnce(t *testing.T) {
	walked := map[string]int{}
	tg := withVols(card(16<<30), drives.Drive{Root: `R:\`})
	Gate(tg, GateInput{Op: OpFormat, Admin: true, HasRescue: func(r string) (bool, bool) { walked[r]++; return false, true }})
	for r, n := range walked {
		if n != 1 {
			t.Errorf("%s walked %d times", r, n)
		}
	}
}

// OA9: the Windows health test locked and dismounted only the card's lettered volumes; one
// mounted without a letter kept the disk, its writes failed and were reported as bad areas. Every
// volume with an extent on the card's disk is taken, lettered or not (the real enumeration needs
// Windows and a card: only this matching is tested).
func TestHealthTestTakesLetterlessVolumesOfTheDisk(t *testing.T) {
	vols := map[string][]uint32{
		`\\?\Volume{c-system}\`:     {0},
		`\\?\Volume{card-letter}\`:  {7},
		`\\?\Volume{card-folder}\`:  {7}, // mounted in a folder, no letter
		`\\?\Volume{spanned}\`:      {3, 7},
		`\\?\Volume{empty-reader}\`: nil,
	}
	enum := func() ([]string, error) {
		var out []string
		for v := range vols {
			out = append(out, v)
		}
		return out, nil
	}
	extents := func(v string) ([]uint32, error) {
		if vols[v] == nil {
			return nil, errors.New("not ready")
		}
		return vols[v], nil
	}
	got, err := volumesOnDisk(enum, extents, 7)
	slices.Sort(got)
	want := []string{`\\?\Volume{card-folder}\`, `\\?\Volume{card-letter}\`, `\\?\Volume{spanned}\`}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v err %v", got, err)
	}
	if n, ok := diskNumber(`\\.\PhysicalDrive12`); !ok || n != 12 {
		t.Fatalf("disk number %d %v", n, ok)
	}
	if _, ok := diskNumber(`\\.\MOCK7`); ok {
		t.Fatal("not a physical disk taken for one")
	}
}

// OA10: on Windows a repair or format is left to finish when Stop is pressed; its result was
// thrown away and the run reported as stopped, though the card was repaired or formatted.
func TestStopDuringAWindowsToolReportsWhatItDid(t *testing.T) {
	old := Exec
	defer func() { Exec = old }()
	ctx, cancel := context.WithCancel(context.Background())
	Exec = func(context.Context, []string, io.Writer) error { cancel(); return nil } // Stop, then it exits 0
	var logged []string
	env := henv()
	env.Log = func(_, msg string) { logged = append(logged, msg) }
	f := &Fix{Op: OpFormat, Target: uniqueCard(16 << 30), GOOS: "windows", Admin: func() bool { return true }}
	res, err := f.Run(ctx, env, job.Plan{})
	if err != nil || res.Counters["DONE"] != 1 || !strings.Contains(strings.Join(logged, "|"), "Stop waited") {
		t.Fatalf("res %+v err %v log %q", res, err, logged)
	}
	// a check is stopped for real: that is "stopped"
	ctx, cancel = context.WithCancel(context.Background())
	Exec = func(c context.Context, _ []string, _ io.Writer) error { cancel(); return c.Err() }
	f.Op = OpCheck
	if _, err := f.Run(ctx, env, job.Plan{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("check: err %v", err)
	}
}

// OA13: a stopped test whose every block before its checkpoint was in a bad area had nothing to
// check the card against, and resumed anyway: another card of the same size inherited its bad
// areas ("Replace this card"). With nothing to compare it starts over.
func TestHealthResumeWithNothingToCompareStartsOver(t *testing.T) {
	const size = 8 << 20
	store := filepath.Join(t.TempDir(), "h.db")
	st, err := job.OpenStore(store)
	if err != nil {
		t.Fatal(err)
	}
	job.Put(st, hBucket, "state", hstate{Seed: 12345, Size: size, Phase: "write", Pos: 4 << 20, Bad: []Range{{0, 4 << 20}}})
	st.Close()
	rep, err := HealthTest(context.Background(), newMem(size, 0), size, henv(), HealthOptions{StorePath: store})
	if err != nil || len(rep.Bad) != 0 {
		t.Fatalf("a healthy card inherited the stopped test's bad areas: %+v err %v", rep.Bad, err)
	}
}
