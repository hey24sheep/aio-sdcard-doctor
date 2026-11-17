package drives

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// readMounts reads mountinfo (it has device numbers), else the plain mount table.
func readMounts() []mount {
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer f.Close()
		if ms := parseMountInfo(f); len(ms) > 0 {
			return ms
		}
	}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcMounts(f)
}

// rootDevice is the major:minor of the device holding / ("" if unknown).
func rootDevice() string {
	var st unix.Stat_t
	if unix.Stat("/", &st) != nil {
		return ""
	}
	dev := uint64(st.Dev)
	return fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
}

// devName resolves a mount's device to the name sysfs knows it by (/dev/root → /dev/mmcblk0p2).
func devName(m mount) string {
	if m.devno != "" {
		if t, err := os.Readlink("/sys/dev/block/" + m.devno); err == nil {
			return "/dev/" + filepath.Base(t)
		}
	}
	return m.dev
}

// byLink maps "/dev/sdb1" → name for the symlinks in /dev/disk/by-uuid or by-label.
func byLink(dir string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
		if err == nil {
			out[t] = decodeUdev(e.Name()) // by-label escapes spaces as \x20
		}
	}
	return out
}

// parentDisk maps a partition name to its whole disk via /sys/class/block/<part>/.. .
func parentDisk(dev string) string {
	name := filepath.Base(dev)
	if _, err := os.Stat("/sys/class/block/" + name + "/partition"); err != nil {
		return name
	}
	t, err := filepath.EvalSymlinks("/sys/class/block/" + name)
	if err != nil {
		return name
	}
	return filepath.Base(filepath.Dir(t))
}

// List returns mounted block-device volumes.
func List() ([]Drive, error) {
	mounts := readMounts()
	uuids, labels := byLink("/dev/disk/by-uuid"), byLink("/dev/disk/by-label")
	devs := map[string]Device{}
	for _, d := range parseSysfs(os.DirFS("/sys"), nil, "") {
		devs[d.PhysicalID] = d
	}
	root := rootDevice()
	var out []Drive
	for _, m := range mounts {
		st, ok, answered := probeWithin("statfs:"+m.dir, probeTimeout, func() (unix.Statfs_t, bool) {
			var st unix.Statfs_t
			return st, unix.Statfs(m.dir, &st) == nil
		})
		name := devName(m)
		disk := parentDisk(name)
		dev := devs[disk]
		if !answered { // a dying card that doesn't answer: listed, never waited for
			d := notResponding(m.dir, name)
			d.PhysicalID, d.Removable, d.ID.PhysicalID = disk, dev.Removable, disk
			out = append(out, d)
			continue
		}
		if !ok {
			continue
		}
		d := Drive{Root: m.dir, FS: m.fs, Label: labels[name], Device: name, PhysicalID: disk,
			System: systemMount[m.dir] || m.devno != "" && m.devno == root,
			Total:  st.Blocks * uint64(st.Bsize), Free: st.Bavail * uint64(st.Bsize),
			WriteProtected: m.ro || dev.ReadOnly, Removable: dev.Removable}
		d.ID = Identity{PhysicalID: disk, DeviceSerial: dev.Serial, Model: dev.Model, Size: uint64(dev.Size),
			FSSerial: uuids[name], Label: d.Label}
		out = append(out, d)
	}
	return out, nil
}

// ListDevices returns physical disks from /sys/block, including unmounted ones.
func ListDevices() ([]Device, error) {
	devs := parseSysfs(os.DirFS("/sys"), mountMap(readMounts()), rootDevice())
	return udevVolumes(devs, byLink("/dev/disk/by-uuid"), byLink("/dev/disk/by-label")), nil
}
