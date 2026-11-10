package drives

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func readMounts() []mount {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcMounts(f)
}

// byLink maps "/dev/sdb1" → name for the symlinks in /dev/disk/by-uuid or by-label.
func byLink(dir string) map[string]string {
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		t, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
		if err == nil {
			out[t] = unescapeMount(strings.ReplaceAll(e.Name(), `\x`, `\0x`)) // by-label escapes spaces as \x20
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
	for _, d := range parseSysfs(os.DirFS("/sys"), nil) {
		devs[d.PhysicalID] = d
	}
	var out []Drive
	for _, m := range mounts {
		var st unix.Statfs_t
		if unix.Statfs(m.dir, &st) != nil {
			continue
		}
		disk := parentDisk(m.dev)
		dev := devs[disk]
		d := Drive{Root: m.dir, FS: m.fs, Label: labels[m.dev], Device: m.dev, System: m.dir == "/", PhysicalID: disk,
			Total: st.Blocks * uint64(st.Bsize), Free: st.Bavail * uint64(st.Bsize),
			WriteProtected: m.ro || dev.ReadOnly, Removable: dev.Removable}
		d.ID = Identity{PhysicalID: disk, DeviceSerial: dev.Serial, Model: dev.Model, Size: uint64(dev.Size),
			FSSerial: uuids[m.dev], Label: d.Label}
		out = append(out, d)
	}
	return out, nil
}

// ListDevices returns physical disks from /sys/block, including unmounted ones.
func ListDevices() ([]Device, error) {
	ms := map[string]mount{}
	for _, m := range readMounts() {
		ms[m.dev] = m
	}
	return parseSysfs(os.DirFS("/sys"), ms), nil
}
