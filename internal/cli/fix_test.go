package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
)

// Every fix test uses a made-up card "Q:" on a made-up disk. fix.Exec and fix.OpenRaw are traps
// by default (cli_test.go TestMain): a test that reaches a real tool or device fails.

type fixRig struct {
	ran  [][]string
	vol  drives.Drive
	disk drives.Device
}

func mockCard(t *testing.T) *fixRig {
	rig := &fixRig{
		vol:  drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "exFAT", Total: 64 << 30, Removable: true, PhysicalID: "MOCK7", Device: "/dev/mock1"},
		disk: drives.Device{Path: `\\.\MOCK7`, Model: "SD Reader", Bus: "usb", Size: 64 << 30, Removable: true, PhysicalID: "MOCK7"},
	}
	rig.disk.Volumes = []drives.Drive{rig.vol}
	oldLookup, oldList, oldExec, oldAdmin, oldGOOS := lookupDrive, listDevicesFn, fix.Exec, isAdmin, fixGOOS
	t.Cleanup(func() {
		lookupDrive, listDevicesFn, fix.Exec, isAdmin, fixGOOS = oldLookup, oldList, oldExec, oldAdmin, oldGOOS
	})
	lookupDrive = func(p string) (drives.Drive, error) {
		// off Windows "Q:" is a relative path, which the CLI makes absolute before the lookup
		if strings.EqualFold(strings.TrimRight(p, `\/`), "Q:") || p == normFrom("Q:") {
			return rig.vol, nil
		}
		return drives.Drive{Root: `C:\`, System: true, PhysicalID: "MOCK0"}, nil
	}
	listDevicesFn = func() ([]drives.Device, error) { return []drives.Device{rig.disk}, nil }
	fix.Exec = func(_ context.Context, argv []string, _ io.Writer) error { rig.ran = append(rig.ran, argv); return nil }
	isAdmin = func() bool { return true }
	fixGOOS = "windows"
	return rig
}

func TestFixFormatTypedConfirm(t *testing.T) {
	rig := mockCard(t)
	code, out, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes", "--confirm-label", "CAMERA", "--confirm-size", "64.0 GB")
	if code != 0 || len(rig.ran) != 1 || !slices.Equal(rig.ran[0], []string{"format", "Q:", "/FS:exFAT", "/Q", "/V:CAMERA", "/Y"}) {
		t.Fatalf("exit %d ran %v\n%s\n%s", code, rig.ran, out, errOut)
	}
	if !strings.Contains(out, "ERASE EVERYTHING") || !strings.Contains(out, "No verified rescue") {
		t.Fatalf("summary/warning missing:\n%s", out)
	}
}

func TestFixWrongConfirmRefused(t *testing.T) {
	rig := mockCard(t)
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes", "--confirm-label", "camera", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "CAMERA") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

func TestFixNeedsConfirmFlagsNonTTY(t *testing.T) {
	rig := mockCard(t)
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes")
	if code != ExitUsage || len(rig.ran) != 0 || !strings.Contains(errOut, "--confirm-label") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

func TestFixNotVerifiedNeedsYes(t *testing.T) {
	rig := mockCard(t)
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--confirm-label", "CAMERA", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "Stopped: No verified rescue") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

func TestFixRefusesSystemDisk(t *testing.T) {
	rig := mockCard(t)
	rig.disk.Volumes = append(rig.disk.Volumes, drives.Drive{Root: `C:\`, System: true, PhysicalID: "MOCK7"})
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes", "--confirm-label", "CAMERA", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "system") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

func TestFixCheckNeedsNoTypedConfirm(t *testing.T) {
	rig := mockCard(t)
	code, out, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--check", "--yes")
	if code != 0 || len(rig.ran) != 1 || rig.ran[0][0] != "chkdsk" {
		t.Fatalf("exit %d ran %v\n%s\n%s", code, rig.ran, out, errOut)
	}
}

func TestFixNeedsExactlyOneOp(t *testing.T) {
	mockCard(t)
	for _, args := range [][]string{{"fix", "--drive", "Q:"}, {"fix", "--drive", "Q:", "--check", "--format"}, {"fix", "--check"}} {
		if code, _, _ := cli(t, false, "", args...); code != ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestFixTTYTypedConfirm(t *testing.T) {
	rig := mockCard(t)
	// accept the "no verified rescue" warning, start now, then type label and size
	in := "1\ny\nCAMERA\n64.0 GB\n"
	code, out, errOut := cli(t, true, in, "fix", "--drive", "Q:", "--format")
	if code != 0 || len(rig.ran) != 1 {
		t.Fatalf("exit %d ran %v\n%s\n%s", code, rig.ran, out, errOut)
	}
}

func TestFixHealthOnImage(t *testing.T) {
	rig := mockCard(t)
	rig.disk.Size, rig.vol.Total = 8<<20, 8<<20
	rig.disk.Volumes = []drives.Drive{rig.vol}
	img := filepath.Join(t.TempDir(), "card.img")
	f, _ := os.Create(img)
	f.Truncate(8 << 20)
	f.Close()
	old := fix.OpenRaw
	defer func() { fix.OpenRaw = old }()
	fix.OpenRaw = func(fix.Target) (fix.RW, io.Closer, error) {
		f, err := os.OpenFile(img, os.O_RDWR, 0)
		return f, f, err
	}
	oldDir := fixStoreDir
	fixStoreDir = t.TempDir()
	defer func() { fixStoreDir = oldDir }()
	code, out, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--health-test", "--no-replug", "--yes",
		"--confirm-label", "CAMERA", "--confirm-size", "8.0 MB")
	if code != 0 || !strings.Contains(out, "REAL-CAPACITY-MB") || !strings.Contains(out+errOut, "no bad areas") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestVerifiedNeedsIdentity (M3 review #2): a rescue can't vouch for a card with no identity.
func TestVerifiedNeedsIdentity(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "manifest.json")
	os.WriteFile(man, []byte(`{"Volume":{"FSSerial":"AAAA-1111","Size":34359738368}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "verify_summary_20261110_000000_000.txt"), []byte("ALL FILES MATCH - the copy is a perfect copy.\n"), 0o644)
	var tg fix.Target // a RAW card: no file-system serial, nothing to match
	if ok, why := verifiedRescue(man, tg); ok || !strings.Contains(why, "identity") {
		t.Fatalf("ok %v why %q", ok, why)
	}
	tg.Vol.ID = drives.Identity{FSSerial: "AAAA-1111", Size: 34359738368}
	if ok, why := verifiedRescue(man, tg); !ok {
		t.Fatalf("same card: %q", why)
	}
}

// TestFixCardSwappedBeforeRun (M3 review #9): the card is looked up again just before anything
// runs; a different card in the slot stops the run.
func TestFixCardSwappedBeforeRun(t *testing.T) {
	rig := mockCard(t)
	rig.disk.Serial = "AAAA"
	calls := 0
	listDevicesFn = func() ([]drives.Device, error) {
		calls++
		d := rig.disk
		if calls > 1 {
			d.Serial = "BBBB" // swapped while the user was typing
		}
		return []drives.Device{d}, nil
	}
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--format", "--yes", "--confirm-label", "CAMERA", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "changed") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

// Final review C1: a rescue that couldn't read some files or folders doesn't count as verified —
// formatting the card would erase what is still only on it.
func TestVerifiedRefusesIncompleteRescue(t *testing.T) {
	dir := t.TempDir()
	man := filepath.Join(dir, "manifest.json")
	os.WriteFile(man, []byte(`{"Volume":{"FSSerial":"AAAA-1111"},"Entries":[`+
		`{"Source":"DCIM/a.jpg","Status":"OK"},{"Source":"DCIM/100BROKE/","Status":"FAILED"}]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "verify_summary_20261110_000000_000.txt"), []byte("ALL FILES MATCH - the copy is a perfect copy.\n"), 0o644)
	var tg fix.Target
	tg.Vol.ID = drives.Identity{FSSerial: "AAAA-1111"}
	if ok, why := verifiedRescue(man, tg); ok || !strings.Contains(why, "couldn't read") {
		t.Fatalf("ok %v why %q", ok, why)
	}
}

// Adversarial pass A9: --drive accepted any path and acted on the whole volume holding it
// ("--drive E:\DCIM", "--drive ."). Card tools take the volume itself, never a folder on it.
func TestFixDriveMustBeTheVolume(t *testing.T) {
	rig := mockCard(t)
	lookupDrive = func(string) (drives.Drive, error) { return rig.vol, nil } // a folder on the card
	code, _, errOut := cli(t, false, "", "fix", "--drive", "Q:/DCIM", "--check")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "not a folder") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

// Adversarial pass A10: with --yes (a script) the "unplug and reinsert" step was answered
// Continue without anyone unplugging, while the summary said you'd be asked. Scripts skip it.
func TestFixHealthScriptSkipsReplug(t *testing.T) {
	rig := mockCard(t)
	rig.disk.Size, rig.vol.Total = 8<<20, 8<<20
	rig.disk.Volumes = []drives.Drive{rig.vol}
	img := filepath.Join(t.TempDir(), "card.img")
	f, _ := os.Create(img)
	f.Truncate(8 << 20)
	f.Close()
	old := fix.OpenRaw
	defer func() { fix.OpenRaw = old }()
	fix.OpenRaw = func(fix.Target) (fix.RW, io.Closer, error) {
		f, err := os.OpenFile(img, os.O_RDWR, 0)
		return f, f, err
	}
	oldDir := fixStoreDir
	fixStoreDir = t.TempDir()
	defer func() { fixStoreDir = oldDir }()
	code, out, errOut := cli(t, false, "", "fix", "--drive", "Q:", "--health-test", "--yes",
		"--confirm-label", "CAMERA", "--confirm-size", "8.0 MB")
	if code != 0 || strings.Contains(out, "unplug") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// Adversarial pass A11/A12: on Linux the card must be unmounted for format, and an unmounted
// volume was listed without a label — Format renamed every card "CARD" and the confirmation
// asked for the device name. Its boot sector says what it is called (and its serial).
func TestUnmountedLinuxCardKeepsItsLabel(t *testing.T) {
	rig := mockCard(t)
	tg := fix.Target{Vol: rig.vol, Disk: rig.disk}
	tg.Vol.Root, tg.Vol.Label, tg.Vol.FS = "", "", "" // unmounted, as Linux lists it
	old := probeVolume
	defer func() { probeVolume = old }()
	probeVolume = func(dev string) (fsread.Volume, bool) {
		return fsread.Volume{FS: "fat32", Label: "CAMERA", Serial: "1234-ABCD"}, dev == tg.Vol.Device
	}
	describeUnmounted(&tg)
	argv, err := fix.Command("linux", fix.OpFormat, tg, "")
	if fix.ConfirmLabel(tg) != "CAMERA" || tg.Vol.ID.FSSerial == "" || err != nil || !slices.Contains(argv, "CAMERA") {
		t.Fatalf("label %q serial %q format %v %v", fix.ConfirmLabel(tg), tg.Vol.ID.FSSerial, argv, err)
	}
}

// Adversarial pass A13: naming a whole disk with several partitions repaired or formatted only
// the first one. Check, repair and format ask for the partition; the health test takes the disk.
func TestWholeDiskWithPartitionsNeedsOne(t *testing.T) {
	rig := mockCard(t)
	second := rig.vol
	second.Root, second.Label, second.Device = `R:\`, "SECOND", "/dev/mock2"
	rig.disk.Volumes = []drives.Drive{rig.vol, second}
	rig.disk.Path = `\\.\PhysicalDrive7`
	code, _, errOut := cli(t, false, "", "fix", "--drive", rig.disk.Path, "--format", "--yes", "--confirm-label", "CAMERA", "--confirm-size", "64.0 GB")
	if code == 0 || len(rig.ran) != 0 || !strings.Contains(errOut, "partitions") {
		t.Fatalf("exit %d ran %v: %s", code, rig.ran, errOut)
	}
}

// Adversarial pass A14: no answer at the label question exited 1 ("error"), at "Start now?" 2.
// No answer (EOF) is 2 everywhere: nothing was started.
func TestNoAnswerExitCodeIsConsistent(t *testing.T) {
	mockCard(t)
	code, out, errOut := cli(t, true, "1\ny\n", "fix", "--drive", "Q:", "--format") // past the warning, start, no label
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d:\n%s\n%s", code, ExitUsage, out, errOut)
	}
}
