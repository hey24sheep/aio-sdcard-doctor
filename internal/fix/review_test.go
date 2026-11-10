package fix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

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
		{"linux", OpRepair, "fat32", exitErr(1), false, true}, // fsck: corrected
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
