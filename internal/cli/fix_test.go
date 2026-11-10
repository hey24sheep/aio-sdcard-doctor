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
		if strings.EqualFold(strings.TrimRight(p, `\/`), "Q:") {
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
