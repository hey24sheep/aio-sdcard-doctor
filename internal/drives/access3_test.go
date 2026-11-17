package drives

import (
	"encoding/binary"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// OA8/SA-2: udev names /dev/disk/by-label links with \xHH escapes (hex); they were read as
// octal, so a card labelled "MY CARD" became "MY\0x20CARD" — the label the user had to type.
func TestUdevLabelsDecodeHexEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`MY\x20CARD`:   "MY CARD",
		`NO\x20NAME`:   "NO NAME",
		`CANON_DC`:     "CANON_DC",
		`a\x2fb\x5cc`:  `a/b\c`,
		`Caf\xc3\xa9`:  "Café",
		`odd\xZZ\x2`:   `odd\xZZ\x2`, // not an escape: kept as it is
		`\x20lead\x20`: " lead ",
	} {
		if got := decodeUdev(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// OA5: a card formatted without a partition table (a superfloppy) had a volume only while
// mounted; on Linux it must be unmounted for repair or format, and then it had none — Fix said
// "the card has no partition (as a health test leaves it)", a dead end with the wrong reason.
// When udev found a file system on the whole disk, the disk is its own volume.
func TestUnpartitionedCardIsItsOwnVolume(t *testing.T) {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s + "\n")} }
	sys := fstest.MapFS{
		"block/sdx/size": f("125829120"), "block/sdx/removable": f("1"), "block/sdx/dev": f("8:16"), // superfloppy
		"block/sdy/size": f("125829120"), "block/sdy/removable": f("1"), "block/sdy/dev": f("8:32"), // blank (after a health test)
		"block/sdz/size": f("0"), "block/sdz/removable": f("1"), "block/sdz/dev": f("8:48"), // empty reader slot
		"block/sdw/size": f("125829120"), "block/sdw/removable": f("1"), "block/sdw/dev": f("8:64"),
		"block/sdw/sdw1/size": f("125827072"), "block/sdw/sdw1/dev": f("8:65"), // partitioned, unmounted
	}
	uuids := map[string]string{"/dev/sdx": "1234-ABCD", "/dev/sdz": "9999-0000", "/dev/sdw1": "5555-6666"}
	labels := map[string]string{"/dev/sdx": "MY CARD"}
	by := map[string]Device{}
	for _, d := range udevVolumes(parseSysfs(sys, map[string]mount{}, ""), uuids, labels) {
		by[d.Path] = d
	}
	v := by["/dev/sdx"].Volumes
	if len(v) != 1 || v[0].Device != "/dev/sdx" || v[0].Root != "" || v[0].Label != "MY CARD" ||
		v[0].ID.FSSerial != "1234-ABCD" || v[0].PhysicalID != "sdx" || v[0].Total != 125829120*512 {
		t.Errorf("superfloppy: %+v", v)
	}
	if n := len(by["/dev/sdy"].Volumes); n != 0 {
		t.Errorf("blank card got %d volumes", n)
	}
	if n := len(by["/dev/sdz"].Volumes); n != 0 {
		t.Errorf("empty slot got %d volumes", n)
	}
	if v := by["/dev/sdw"].Volumes; len(v) != 1 || v[0].Device != "/dev/sdw1" {
		t.Errorf("partitioned card: %+v", v)
	}
	// mounted, the superfloppy already has its volume: not listed twice
	m := map[string]mount{"/dev/sdx": {dev: "/dev/sdx", dir: "/media/u/CAM", fs: "vfat", devno: "8:16"}}
	for _, d := range udevVolumes(parseSysfs(sys, m, ""), uuids, labels) {
		if d.Path == "/dev/sdx" && (len(d.Volumes) != 1 || d.Volumes[0].Root != "/media/u/CAM") {
			t.Errorf("mounted superfloppy: %+v", d.Volumes)
		}
	}
}

// SA-7: listing drives asked each volume for its file system and size with no deadline: a dying
// card that doesn't answer froze the list (and the window refreshing it). A volume that doesn't
// answer in time is listed as not responding; a listing repeated while it is still stuck doesn't
// start another probe of it.
func TestStuckVolumeDoesNotFreezeTheList(t *testing.T) {
	stuck := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	probe := func() (Drive, bool) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-stuck
		return Drive{Root: "Z:", Label: "LATE"}, true
	}
	for i := 0; i < 3; i++ {
		start := time.Now()
		if _, ok, answered := probeWithin("Z:", 50*time.Millisecond, probe); ok || answered {
			t.Fatal("a stuck volume answered")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("waited %v", d)
		}
	}
	mu.Lock()
	if calls != 1 {
		t.Errorf("%d probes of one stuck volume", calls)
	}
	mu.Unlock()
	close(stuck)
	time.Sleep(50 * time.Millisecond)
	if d, ok, _ := probeWithin("Z:", time.Second, func() (Drive, bool) { return Drive{Root: "Z:", Label: "BACK"}, true }); !ok || d.Label != "BACK" {
		t.Fatalf("after it answered: %+v %v", d, ok)
	}
	if d := notResponding("Z:", "dev"); !d.RAW || d.Root != "Z:" || d.Device != "dev" {
		t.Fatalf("%+v", d)
	}
}

// OA11: macOS lists a FAT or exFAT card with a UUID it derives, while Windows and a raw read of
// the same card give the boot sector's serial: the folder and raw identities of one card
// differed (a raw resume of a folder rescue refused, two locks for one card). The boot sector's
// serial is what is used.
func TestBootSectorSerial(t *testing.T) {
	sector := func(set func(b []byte)) []byte {
		b := make([]byte, 512)
		b[510], b[511] = 0x55, 0xAA
		binary.LittleEndian.PutUint16(b[11:], 512)
		b[13] = 8
		set(b)
		return b
	}
	fat32 := sector(func(b []byte) { b[66] = 0x29; binary.LittleEndian.PutUint32(b[67:], 0x1234ABCD) })
	fat16 := sector(func(b []byte) {
		binary.LittleEndian.PutUint16(b[22:], 200)
		b[38] = 0x29
		binary.LittleEndian.PutUint32(b[39:], 0x00C0FFEE)
	})
	exfat := make([]byte, 512)
	copy(exfat[3:], "EXFAT   ")
	binary.LittleEndian.PutUint32(exfat[100:], 0xDEADBEEF)
	exfat[510], exfat[511] = 0x55, 0xAA
	ntfs := sector(func(b []byte) { copy(b[3:], "NTFS    "); binary.LittleEndian.PutUint16(b[22:], 0) })
	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		"fat32": {fat32, "1234-ABCD"}, "fat16": {fat16, "00C0-FFEE"}, "exfat": {exfat, "DEAD-BEEF"},
		"ntfs": {ntfs, ""}, "blank": {make([]byte, 512), ""}, "short": {fat32[:100], ""},
	} {
		if got := bootSectorSerial(c.b); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// the same card read as a folder on macOS and raw: one identity
	if NormalizeSerial(bootSectorSerial(fat32)) != NormalizeSerial("1234ABCD") {
		t.Fatal("not comparable with a raw read")
	}
}
