package drives

import (
	"strings"
	"testing"
	"testing/fstest"
)

// A Raspberry Pi booted without an initramfs mounts its root file system as /dev/root, a name
// sysfs doesn't know; its firmware partition is at /boot/firmware.
const piMountInfo = `22 1 179:2 / / rw,noatime shared:1 - ext4 /dev/root rw
24 22 179:1 / /boot/firmware rw,relatime shared:2 - vfat /dev/mmcblk0p1 rw,fmask=0022
25 22 0:21 / /proc rw,nosuid shared:3 - proc proc rw
30 22 8:17 / /media/pi/My\040Card rw,nosuid shared:4 - exfat /dev/sdb1 ro
`

func piSysfs() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s + "\n")} }
	return fstest.MapFS{
		"block/mmcblk0/dev":            f("179:0"),
		"block/mmcblk0/size":           f("62333952"),
		"block/mmcblk0/device/type":    f("SD"),
		"block/mmcblk0/mmcblk0p1/dev":  f("179:1"),
		"block/mmcblk0/mmcblk0p1/size": f("1048576"),
		"block/mmcblk0/mmcblk0p2/dev":  f("179:2"),
		"block/mmcblk0/mmcblk0p2/size": f("61276160"),
		"block/sdb/dev":                f("8:16"),
		"block/sdb/removable":          f("1"),
		"block/sdb/size":               f("124735488"),
		"block/sdb/sdb1/dev":           f("8:17"),
		"block/sdb/sdb1/size":          f("124727296"),
	}
}

func TestParseMountInfo(t *testing.T) {
	ms := parseMountInfo(strings.NewReader(piMountInfo))
	if len(ms) != 3 {
		t.Fatalf("%+v", ms)
	}
	if ms[0].dev != "/dev/root" || ms[0].dir != "/" || ms[0].devno != "179:2" || ms[0].fs != "ext4" {
		t.Errorf("root: %+v", ms[0])
	}
	if ms[2].dir != "/media/pi/My Card" || !ms[2].ro || ms[1].ro {
		t.Errorf("card: %+v", ms[2])
	}
}

func piDisks(t *testing.T, mounts map[string]mount, rootDev string) (sd, usb Device) {
	t.Helper()
	for _, d := range parseSysfs(piSysfs(), mounts, rootDev) {
		switch d.Path {
		case "/dev/mmcblk0":
			sd = d
		case "/dev/sdb":
			usb = d
		}
	}
	if len(sd.Volumes) != 2 || len(usb.Volumes) != 1 {
		t.Fatalf("volumes: sd %+v usb %+v", sd.Volumes, usb.Volumes)
	}
	return sd, usb
}

// The Pi's own SD card is the system disk: matched by device number, never taken for a card.
func TestPiSystemCardFoundByDeviceNumber(t *testing.T) {
	sd, usb := piDisks(t, mountMap(parseMountInfo(strings.NewReader(piMountInfo))), "")
	if !sd.Volumes[0].System || !sd.Volumes[1].System || sd.Volumes[1].Root != "/" || !sd.Mounted {
		t.Errorf("system card: %+v", sd.Volumes)
	}
	if usb.Volumes[0].System || usb.Volumes[0].Root != "/media/pi/My Card" {
		t.Errorf("card in the reader: %+v", usb.Volumes)
	}
}

// Without any mount table, the partition holding / (by its device number) is still the system.
func TestRootDeviceMarksSystem(t *testing.T) {
	sd, usb := piDisks(t, nil, "179:2")
	if !sd.Volumes[1].System || sd.Volumes[0].System || usb.Volumes[0].System {
		t.Errorf("sd %+v usb %+v", sd.Volumes, usb.Volumes)
	}
}
