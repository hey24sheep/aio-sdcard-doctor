package drives

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNormalizeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		for _, in := range []string{"E", "e:", "E:", `E:\`, "e:/", `e:\\`} {
			if got := NormalizeRoot(in); got != `E:\` {
				t.Errorf("%q → %q", in, got)
			}
		}
		if got := NormalizeRoot(`D:\rescue\`); got != `D:\rescue` {
			t.Errorf("folder → %q", got)
		}
		return
	}
	if got := NormalizeRoot("/media/card/"); got != "/media/card" {
		t.Errorf("→ %q", got)
	}
	if got := NormalizeRoot("/"); got != "/" {
		t.Errorf("/ → %q", got)
	}
}

func TestListContainsSystemDrive(t *testing.T) {
	ds, err := List()
	if err != nil {
		t.Fatal(err)
	}
	sys := 0
	for _, d := range ds {
		if d.System {
			sys++
			if d.Total == 0 || d.FS == "" {
				t.Errorf("system drive incomplete: %+v", d)
			}
		}
	}
	if sys != 1 {
		t.Fatalf("%d system drives in %+v", sys, ds)
	}
}

func TestLookupLongestMatch(t *testing.T) {
	old := Lister
	defer func() { Lister = old }()
	sep := string(filepath.Separator)
	a, b := sep+"mnt", filepath.Join(sep+"mnt", "card")
	if runtime.GOOS == "windows" {
		a, b = `C:\`, `C:\mnt\card`
	}
	Lister = func() ([]Drive, error) { return []Drive{{Root: a, Label: "sys"}, {Root: b, Label: "card"}}, nil }
	d, err := Lookup(filepath.Join(b, "DCIM", "x.jpg"))
	if err != nil || d.Label != "card" {
		t.Fatalf("got %+v %v", d, err)
	}
	d, _ = Lookup(filepath.Join(a, "cardx", "y"))
	if d.Label != "sys" {
		t.Fatalf("prefix matched: %+v", d)
	}
}

func TestParseProcMounts(t *testing.T) {
	in := `sysfs /sys sysfs rw 0 0
/dev/nvme0n1p2 / ext4 rw,relatime 0 0
/dev/mmcblk0p1 /media/u/My\040Card vfat rw,nosuid 0 0
/dev/sdb1 /media/u/USB exfat ro 0 0
tmpfs /run tmpfs rw 0 0
`
	ms := parseProcMounts(strings.NewReader(in))
	if len(ms) != 3 {
		t.Fatalf("%+v", ms)
	}
	if ms[1].dir != "/media/u/My Card" || ms[1].fs != "vfat" || ms[1].dev != "/dev/mmcblk0p1" {
		t.Errorf("%+v", ms[1])
	}
	if !ms[2].ro || ms[1].ro {
		t.Errorf("ro flags wrong: %+v", ms)
	}
}

func sysfsFixture() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s + "\n")} }
	return fstest.MapFS{
		"block/mmcblk0/removable":       f("0"),
		"block/mmcblk0/ro":              f("0"),
		"block/mmcblk0/size":            f("124735488"),
		"block/mmcblk0/device/name":     f("SD64G"),
		"block/mmcblk0/device/serial":   f("0x1234abcd"),
		"block/mmcblk0/device/type":     f("SD"),
		"block/mmcblk0/mmcblk0p1/size":  f("124727296"),
		"block/mmcblk0/mmcblk0p1/start": f("8192"),
		"block/mmcblk0/mmcblk0p1/ro":    f("0"),
		"block/sdb/removable":           f("1"),
		"block/sdb/ro":                  f("1"),
		"block/sdb/size":                f("31116288"),
		"block/sdb/device/model":        f("Card Reader    "),
		"block/sdb/device/vendor":       f("Generic"),
		"block/nvme0n1/removable":       f("0"),
		"block/nvme0n1/ro":              f("0"),
		"block/nvme0n1/size":            f("1000215216"),
		"block/nvme0n1/device/model":    f("Samsung SSD"),
		"block/nvme0n1/nvme0n1p2/size":  f("999000000"),
		"block/loop0/size":              f("100"),
		"block/loop0/removable":         f("0"),
	}
}

func TestParseSysfs(t *testing.T) {
	devs := parseSysfs(sysfsFixture(), map[string]mount{"/dev/nvme0n1p2": {dev: "/dev/nvme0n1p2", dir: "/"}})
	byPath := map[string]Device{}
	for _, d := range devs {
		byPath[d.Path] = d
	}
	if _, ok := byPath["/dev/loop0"]; ok {
		t.Error("loop device listed")
	}
	sd := byPath["/dev/mmcblk0"]
	if sd.Size != 124735488*512 || !sd.Removable || sd.Model != "SD64G" || sd.Serial != "0x1234abcd" || len(sd.Volumes) != 1 {
		t.Errorf("mmc: %+v", sd)
	}
	if sd.Mounted {
		t.Error("unmounted card marked mounted")
	}
	usb := byPath["/dev/sdb"]
	if !usb.Removable || !usb.ReadOnly || usb.Model != "Generic Card Reader" || len(usb.Volumes) != 0 {
		t.Errorf("sdb: %+v", usb)
	}
	nv := byPath["/dev/nvme0n1"]
	if nv.Removable || !nv.Mounted || nv.Bus != "nvme" {
		t.Errorf("nvme: %+v", nv)
	}
}

func TestDevicesIncludeUnmounted(t *testing.T) {
	devs := parseSysfs(sysfsFixture(), nil)
	n := 0
	for _, d := range devs {
		if !d.Mounted {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("unmounted devices = %d, want 3", n)
	}
}

func TestParseDiskutilPlist(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "diskutil-info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := parsePlistDict(b)
	if err != nil {
		t.Fatal(err)
	}
	if info.str("MediaName") != "SD Card Reader" || info.str("VolumeUUID") != "6B1D0A2E-1111-3C55-9E0C-AB12CD34EF56" ||
		!info.boolean("RemovableMedia") || info.num("TotalSize") != 63864569856 || info.str("ParentWholeDisk") != "disk4" {
		t.Fatalf("%+v", info)
	}
	b, _ = os.ReadFile(filepath.Join("testdata", "diskutil-list.plist"))
	list, err := parsePlistDict(b)
	if err != nil {
		t.Fatal(err)
	}
	disks := list.list("AllDisksAndPartitions")
	if len(disks) != 2 || disks[1].str("DeviceIdentifier") != "disk4" || len(disks[1].list("Partitions")) != 1 {
		t.Fatalf("%+v", disks)
	}
}

func fp(blocks ...string) []FPBlock {
	var out []FPBlock
	for i, h := range blocks {
		out = append(out, FPBlock{Where: "off:" + string(rune('0'+i)), SHA256: h})
	}
	return out
}

func TestIdentityMatches(t *testing.T) {
	base := Identity{Model: "SD64", Size: 64, FSSerial: "AB12-CD34"}
	a, b := base, base
	a.Fingerprint, b.Fingerprint = fp("1", "2", "3", "4"), fp("1", "2", "3", "4")
	if ok, m, c := a.Matches(b); !ok || m != 4 || c != 4 {
		t.Fatalf("same card: %v %d %d", ok, m, c)
	}
	b.Fingerprint = fp("1", "2", "3", "X")
	if ok, _, _ := a.Matches(b); ok {
		t.Fatal("one mismatching block accepted")
	}
	b = base
	b.FSSerial = "FFFF-0000"
	if ok, _, _ := a.Matches(b); ok {
		t.Fatal("different FS serial accepted")
	}
	noSerial := Identity{Size: 64}
	if ok, _, _ := noSerial.Matches(Identity{Size: 32}); ok {
		t.Fatal("different size accepted when no serial is known")
	}
	if ok, _, _ := base.Matches(base); !ok {
		t.Fatal("no fingerprints yet should match on serial/size/model")
	}
}

func TestResumeRejectsDifferentCardSameModel(t *testing.T) {
	a := Identity{Model: "SanDisk 64GB", Size: 64 << 30, FSSerial: "1111-2222", Fingerprint: fp("a", "b", "c")}
	b := Identity{Model: "SanDisk 64GB", Size: 64 << 30, FSSerial: "3333-4444", Fingerprint: fp("x", "y", "z")}
	if ok, _, _ := a.Matches(b); ok {
		t.Fatal("same model, different card accepted")
	}
}

func TestFingerprintSkipsUnreadableBlocks(t *testing.T) {
	base := Identity{Model: "m", Size: 1, FSSerial: "s"}
	a, b := base, base
	a.Fingerprint = fp("1", "", "3", "4", "5")
	b.Fingerprint = fp("1", "2", "", "4", "5")
	ok, m, c := a.Matches(b)
	if !ok || m != 3 || c != 3 {
		t.Fatalf("%v %d %d", ok, m, c)
	}
	b.Fingerprint = fp("1", "", "", "", "")
	if ok, m, c := a.Matches(b); !ok || m != 1 || c != 1 {
		t.Fatalf("fewer than 3 comparable: all must match: %v %d %d", ok, m, c)
	}
}

func TestIdentityFolderModeNoAdmin(t *testing.T) {
	d, err := Lookup(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.ID.FSSerial == "" && runtime.GOOS == "windows" {
		t.Fatalf("no FS serial without admin: %+v", d.ID)
	}
	if d.ID.LockKey() == "" {
		t.Fatal("empty lock key")
	}
}

// TestSameCardInAnotherReader: Model and DeviceSerial describe the USB reader, not the card.
func TestSameCardInAnotherReader(t *testing.T) {
	a := Identity{Model: "Reader A", DeviceSerial: "111", Size: 64, FSSerial: "AB12-CD34", Fingerprint: fp("1", "2", "3")}
	b := Identity{Model: "Reader B", DeviceSerial: "222", Size: 64, FSSerial: "AB12-CD34", Fingerprint: fp("1", "2", "3")}
	if ok, _, _ := a.Matches(b); !ok {
		t.Fatal("same card moved to another reader rejected")
	}
	if a.LockKey() != b.LockKey() {
		t.Fatal("same card in another reader gets another lock")
	}
}

func TestPhysicalOf(t *testing.T) {
	cases := map[string]string{`\\.\PhysicalDrive7`: "PhysicalDrive7", "/dev/sdb": "sdb", "/dev/mmcblk0": "mmcblk0",
		"/dev/disk4": "disk4", "/dev/rdisk4": "disk4", "/dev/disk4s1": "disk4", "card.img": ""}
	for in, want := range cases {
		if got := physicalOfPath(in); got != want {
			t.Errorf("%s → %q want %q", in, got, want)
		}
	}
}

// TestParseSysfsGateFacts (M3 review #1): Fix's gate needs, per partition, its device path and
// whether it holds the system, and per disk whether LVM/RAID/dm-crypt holds it — mounted or not.
func TestParseSysfsGateFacts(t *testing.T) {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s + "\n")} }
	fx := sysfsFixture()
	fx["block/sda/removable"], fx["block/sda/size"] = f("0"), f("2000000")
	fx["block/sda/sda1/size"] = f("1000000")
	fx["block/sda/sda1/holders/dm-0"] = f("") // an LVM member of the running system, not mounted itself
	fx["block/sdc/removable"], fx["block/sdc/size"] = f("1"), f("1000")
	fx["block/sdc/holders/md0"] = f("") // whole disk in a RAID
	devs := parseSysfs(fx, map[string]mount{
		"/dev/nvme0n1p2": {dev: "/dev/nvme0n1p2", dir: "/", fs: "ext4"},
		"/dev/sdb":       {dev: "/dev/sdb", dir: "/media/card", fs: "vfat"}, // a superfloppy card
	})
	by := map[string]Device{}
	for _, d := range devs {
		by[d.Path] = d
	}
	nv := by["/dev/nvme0n1"]
	if len(nv.Volumes) != 1 || !nv.Volumes[0].System || nv.Volumes[0].Device != "/dev/nvme0n1p2" {
		t.Errorf("system partition: %+v", nv.Volumes)
	}
	if v := by["/dev/mmcblk0"].Volumes; len(v) != 1 || v[0].Device != "/dev/mmcblk0p1" || v[0].Root != "" {
		t.Errorf("unmounted partition: %+v", v)
	}
	if by["/dev/sda"].InUse == "" || by["/dev/sdc"].InUse == "" || by["/dev/mmcblk0"].InUse != "" {
		t.Errorf("in use: sda %q sdc %q mmc %q", by["/dev/sda"].InUse, by["/dev/sdc"].InUse, by["/dev/mmcblk0"].InUse)
	}
	if v := by["/dev/sdb"].Volumes; len(v) != 1 || v[0].Device != "/dev/sdb" || v[0].Root != "/media/card" {
		t.Errorf("superfloppy: %+v", v)
	}
}
