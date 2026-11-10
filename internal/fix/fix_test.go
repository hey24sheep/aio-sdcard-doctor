package fix

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// Every target here is made up: no test looks at, opens or runs anything against a real disk.

func card(size uint64) Target {
	return Target{
		Vol:  drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "exFAT", Total: size, Removable: true, PhysicalID: "MOCK7", Device: "/dev/mock1"},
		Disk: drives.Device{Path: `\\.\MOCK7`, Model: "SD Reader", Bus: "usb", Size: int64(size), Removable: true, PhysicalID: "MOCK7"},
	}
}

func withVols(t Target, vols ...drives.Drive) Target {
	t.Disk.Volumes = append([]drives.Drive{t.Vol}, vols...)
	return t
}

func gate(t Target, in GateInput) ([]string, error) {
	if in.Op == "" {
		in.Op = OpFormat
	}
	if in.HasRescue == nil {
		in.HasRescue = func(string) bool { return false }
	}
	return Gate(t, in)
}

func refused(err error, want string) bool {
	var r *Refused
	return errors.As(err, &r) && strings.Contains(r.Reason, want)
}

func TestGateSystemDriveRefused(t *testing.T) {
	sys := drives.Drive{Root: `C:\`, System: true, PhysicalID: "MOCK7"}
	_, err := gate(withVols(card(64<<30), sys), GateInput{Verified: true, AllowFixed: true, Admin: true})
	if !refused(err, "system") {
		t.Fatalf("err %v", err)
	}
}

func TestGateRefusesDiskWithDestination(t *testing.T) {
	other := drives.Drive{Root: `R:\`, PhysicalID: "MOCK7"}
	_, err := gate(withVols(card(64<<30), other), GateInput{Verified: true, Admin: true,
		HasRescue: func(root string) bool { return root == `R:\` }})
	if !refused(err, "rescue") {
		t.Fatalf("err %v", err)
	}
}

func TestGateRefusesBinaryDisk(t *testing.T) {
	_, err := gate(card(64<<30), GateInput{Verified: true, Admin: true, BinaryDisk: "MOCK7"})
	if !refused(err, "this program") {
		t.Fatalf("err %v", err)
	}
}

func TestGateFixedNeedsFlag(t *testing.T) {
	c := card(64 << 30)
	c.Disk.Removable = false
	if _, err := gate(c, GateInput{Verified: true, Admin: true}); !refused(err, "--allow-fixed") {
		t.Fatalf("err %v", err)
	}
	if _, err := gate(c, GateInput{Verified: true, Admin: true, AllowFixed: true}); err != nil {
		t.Fatalf("with --allow-fixed: %v", err)
	}
}

func TestGateUSBHardDiskIsFixed(t *testing.T) {
	c := card(500 << 30)
	c.Disk.Removable = false // external USB HDD: bus usb, removable-media flag false
	if _, err := gate(c, GateInput{Verified: true, Admin: true}); !refused(err, "--allow-fixed") {
		t.Fatalf("err %v", err)
	}
}

func TestGateFixedByBusType(t *testing.T) {
	for _, bus := range []string{"nvme", "sata", "sas", "raid"} {
		c := card(64 << 30)
		c.Disk.Bus = bus // even if it claims removable
		if _, err := gate(c, GateInput{Verified: true, Admin: true}); !refused(err, "--allow-fixed") {
			t.Errorf("%s: %v", bus, err)
		}
	}
	big := card(3 << 40)
	if _, err := gate(big, GateInput{Verified: true, Admin: true}); !refused(err, "--allow-fixed") {
		t.Errorf("3 TB: %v", err)
	}
}

func TestGate1TBSDCardAllowed(t *testing.T) {
	c := card(1 << 40)
	c.Disk.Bus = "sd"
	if _, err := gate(c, GateInput{Verified: true, Admin: true}); err != nil {
		t.Fatal(err)
	}
}

func TestGateNonAdminElevates(t *testing.T) {
	if _, err := gate(card(64<<30), GateInput{Verified: true}); !errors.Is(err, ErrNeedsAdmin) {
		t.Fatalf("err %v", err)
	}
}

func TestGateWriteProtected(t *testing.T) {
	c := card(64 << 30)
	c.Disk.ReadOnly = true
	if _, err := gate(c, GateInput{Verified: true, Admin: true}); !refused(err, "lock switch") {
		t.Fatalf("err %v", err)
	}
	if _, err := gate(c, GateInput{Op: OpCheck, Verified: true, Admin: true}); err != nil {
		t.Fatalf("a check only reads: %v", err)
	}
}

func TestGateLocked(t *testing.T) {
	c := card(64 << 30)
	c.Vol.Locked = true
	if _, err := gate(c, GateInput{Verified: true, Admin: true}); !refused(err, "unlock") {
		t.Fatalf("err %v", err)
	}
}

func TestGateNoManifestWarns(t *testing.T) {
	ws, err := gate(card(64<<30), GateInput{Admin: true})
	if err != nil || len(ws) != 1 || !strings.Contains(ws[0], "Rescue") {
		t.Fatalf("%v %v", ws, err)
	}
	if ws, _ := gate(card(64<<30), GateInput{Op: OpCheck, Admin: true}); len(ws) != 0 {
		t.Fatalf("a check warns: %v", ws)
	}
}

func TestConfirm(t *testing.T) {
	c := card(64 << 30)
	if err := Confirm(c, "CAMERA", "64.0 GB"); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(c, "camera", "64.0 GB"); err == nil {
		t.Fatal("label is case-sensitive")
	}
	noLabel := c
	noLabel.Vol.Label = ""
	if err := Confirm(noLabel, "Q:", "64.0 GB"); err != nil {
		t.Fatalf("no label → drive letter: %v", err)
	}
}

func TestConfirmNeedsLabelAndSize(t *testing.T) {
	c := card(64 << 30)
	if err := Confirm(c, "CAMERA", ""); err == nil {
		t.Fatal("size not required")
	}
	if err := Confirm(c, "CAMERA", "32.0 GB"); err == nil {
		t.Fatal("wrong size accepted")
	}
	if err := Confirm(c, "", "64.0 GB"); err == nil {
		t.Fatal("label not required")
	}
	if err := Confirm(c, "CAMERA", " 64.0gb "); err != nil {
		t.Fatalf("spacing/case of the size: %v", err)
	}
}

func TestCommandsPerOS(t *testing.T) {
	small, big := card(16<<30), card(128<<30)
	small.Vol.FS, big.Vol.FS = "FAT32", "exFAT"
	unmounted := func(t Target) Target { t.Vol.Root = ""; return t }
	cases := []struct {
		goos, op string
		t        Target
		want     []string
	}{
		{"windows", OpCheck, small, []string{"chkdsk", "Q:"}},
		{"windows", OpRepair, small, []string{"chkdsk", "Q:", "/f", "/x"}},
		{"windows", OpFormat, small, []string{"format", "Q:", "/FS:FAT32", "/Q", "/V:CAMERA", "/Y"}},
		{"windows", OpFormat, big, []string{"format", "Q:", "/FS:exFAT", "/Q", "/V:CAMERA", "/Y"}},
		{"darwin", OpCheck, small, []string{"diskutil", "verifyVolume", "/dev/mock1"}},
		{"darwin", OpRepair, small, []string{"diskutil", "repairVolume", "/dev/mock1"}},
		{"darwin", OpFormat, small, []string{"diskutil", "eraseVolume", "FAT32", "CAMERA", "/dev/mock1"}},
		{"darwin", OpFormat, big, []string{"diskutil", "eraseVolume", "ExFAT", "CAMERA", "/dev/mock1"}},
		{"linux", OpCheck, small, []string{"fsck.vfat", "-n", "/dev/mock1"}},
		{"linux", OpRepair, unmounted(small), []string{"fsck.vfat", "-a", "/dev/mock1"}},
		{"linux", OpCheck, big, []string{"fsck.exfat", "-n", "/dev/mock1"}},
		{"linux", OpRepair, unmounted(big), []string{"fsck.exfat", "-y", "/dev/mock1"}},
		{"linux", OpFormat, unmounted(small), []string{"mkfs.vfat", "-F", "32", "-n", "CAMERA", "/dev/mock1"}},
		{"linux", OpFormat, unmounted(big), []string{"mkfs.exfat", "-n", "CAMERA", "/dev/mock1"}},
	}
	for _, c := range cases {
		got, err := Command(c.goos, c.op, c.t, "")
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s %s: %v %v, want %v", c.goos, c.op, got, err, c.want)
		}
	}
	if _, err := Command("linux", OpFormat, small, ""); err == nil || !strings.Contains(err.Error(), "unmount") {
		t.Errorf("linux format of a mounted card: %v", err)
	}
	if got, _ := Command("windows", OpFormat, small, "my card!!"); !slices.Contains(got, "/V:MY CARD") {
		t.Errorf("label not cleaned: %v", got)
	}
}

func TestAvailablePerOSAndFS(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	have := map[string]bool{"fsck.vfat": true, "mkfs.vfat": true}
	lookPath = func(name string) (string, error) {
		if have[name] {
			return "/usr/sbin/" + name, nil
		}
		return "", errors.New("not found")
	}
	type want struct{ check, repair, format, health bool }
	cases := []struct {
		goos, fs string
		w        want
		reason   string
	}{
		{"windows", "fat32", want{true, true, true, true}, ""},
		{"windows", "exfat", want{true, true, true, true}, ""},
		{"windows", "ntfs", want{true, true, true, true}, ""},
		{"windows", "raw", want{false, false, true, true}, "isn't recognised"},
		{"darwin", "ntfs", want{false, false, true, true}, "no NTFS repair tool on macOS"},
		{"darwin", "apfs", want{false, false, true, true}, "use Disk Utility"},
		{"darwin", "exfat", want{true, true, true, true}, ""},
		{"linux", "fat32", want{true, true, true, true}, ""},
		{"linux", "exfat", want{false, false, true, true}, "exfatprogs"},
	}
	for _, c := range cases {
		cs := Available(c.goos, card(16<<30), c.fs)
		got := want{cs[0].Enabled, cs[1].Enabled, cs[2].Enabled, cs[3].Enabled}
		if got != c.w {
			t.Errorf("%s/%s: %+v", c.goos, c.fs, cs)
		}
		reasons := ""
		for _, x := range cs {
			if !x.Enabled && x.Reason == "" || len(x.Reason) > 60 {
				t.Errorf("%s/%s %s: reason %q", c.goos, c.fs, x.Name, x.Reason)
			}
			reasons += x.Reason + "|"
		}
		if !strings.Contains(reasons, c.reason) {
			t.Errorf("%s/%s: reasons %q lack %q", c.goos, c.fs, reasons, c.reason)
		}
	}
	// a big card formats as exFAT: on Linux that needs mkfs.exfat
	if cs := Available("linux", card(128<<30), "exfat"); cs[2].Enabled || !strings.Contains(cs[2].Reason, "mkfs.exfat") {
		t.Errorf("linux exfat format without mkfs.exfat: %+v", cs[2])
	}
	wp := card(16 << 30)
	wp.Vol.WriteProtected = true
	cs := Available("windows", wp, "fat32")
	if !cs[0].Enabled || cs[1].Enabled || cs[2].Enabled || cs[3].Enabled || !strings.Contains(cs[2].Reason, "lock switch") {
		t.Errorf("write-protected: %+v", cs)
	}
}

func TestHasRescueFindsDestinations(t *testing.T) {
	root := t.TempDir()
	if HasRescue(root) {
		t.Fatal("empty disk has a rescue")
	}
	os.MkdirAll(filepath.Join(root, "my rescue", "Reports"), 0o755)
	os.WriteFile(filepath.Join(root, "my rescue", "Reports", "state.db"), nil, 0o644)
	if !HasRescue(root) {
		t.Fatal("rescue one folder down not found")
	}
}
