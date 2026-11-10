package drives

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func cstring(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// diskutil runs `diskutil <args> -plist` with a deadline (it can hang on a dying disk).
func diskutil(args ...string) plist {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", append(args, "-plist")...).Output()
	if err != nil {
		return plist{}
	}
	p, err := parsePlistDict(out)
	if err != nil {
		return plist{}
	}
	return p
}

// List returns mounted local volumes.
func List() ([]Drive, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil, err
	}
	var out []Drive
	for _, st := range buf[:n] {
		from := cstring(st.Mntfromname[:])
		if !strings.HasPrefix(from, "/dev/disk") {
			continue
		}
		root := cstring(st.Mntonname[:])
		info := diskutil("info", from)
		whole := info.str("ParentWholeDisk")
		d := Drive{Root: root, FS: cstring(st.Fstypename[:]), Label: info.str("VolumeName"), Device: from,
			Total: st.Blocks * uint64(st.Bsize), Free: st.Bavail * uint64(st.Bsize),
			System: root == "/", PhysicalID: whole,
			WriteProtected: st.Flags&unix.MNT_RDONLY != 0 || info["WritableMedia"] != nil && !info.boolean("WritableMedia"),
			Removable:      info.boolean("RemovableMedia") || info.boolean("Ejectable") && !info.boolean("Internal")}
		d.ID = Identity{PhysicalID: whole, Model: info.str("MediaName"), Size: uint64(info.num("TotalSize")),
			FSSerial: info.str("VolumeUUID"), Label: d.Label}
		out = append(out, d)
	}
	return out, nil
}

// ListDevices returns whole disks from `diskutil list`, including unmounted ones.
func ListDevices() ([]Device, error) {
	vols, _ := List()
	byWhole := map[string][]Drive{}
	for _, v := range vols {
		byWhole[v.PhysicalID] = append(byWhole[v.PhysicalID], v)
	}
	var out []Device
	for _, d := range diskutil("list").list("AllDisksAndPartitions") {
		id := d.str("DeviceIdentifier")
		info := diskutil("info", id)
		dev := Device{Path: "/dev/" + id, PhysicalID: id, Size: d.num("Size"), Model: info.str("MediaName"),
			Removable: info.boolean("RemovableMedia"), Bus: strings.ToLower(info.str("BusProtocol")),
			ReadOnly: !info.boolean("WritableMedia"), Volumes: byWhole[id]}
		dev.Mounted = len(dev.Volumes) > 0
		out = append(out, dev)
	}
	return out, nil
}
