package drives

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	ioctlStorageQueryProperty   = 0x2D1400
	ioctlStorageGetDeviceNumber = 0x2D1080
	ioctlDiskGetGeometryEx      = 0x700A0 // FILE_ANY_ACCESS, unlike GET_LENGTH_INFO
	ioctlDiskIsWritable         = 0x70024
	ioctlVolumeGetDiskExtents   = 0x560000
	fileReadOnlyVolume          = 0x00080000
	errUnrecognizedVolume       = windows.Errno(1005)
)

// busNames maps STORAGE_BUS_TYPE to a short name.
var busNames = map[uint32]string{1: "scsi", 3: "ata", 7: "usb", 8: "raid", 10: "sas", 11: "sata", 12: "sd", 13: "mmc", 17: "nvme"}

// openQuery opens a device with zero access: enough for property ioctls, never for reads/writes.
func openQuery(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
}

func ioctl(h windows.Handle, code uint32, in []byte, out []byte) (int, error) {
	var n uint32
	var inp, outp *byte
	if len(in) > 0 {
		inp = &in[0]
	}
	if len(out) > 0 {
		outp = &out[0]
	}
	err := windows.DeviceIoControl(h, code, inp, uint32(len(in)), outp, uint32(len(out)), &n, nil)
	return int(n), err
}

type descriptor struct {
	model, serial, bus string
	removable          bool
}

func cstr(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	s := b[off:]
	if i := strings.IndexByte(string(s), 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(string(s))
}

func queryDescriptor(h windows.Handle) (descriptor, error) {
	q := make([]byte, 12) // STORAGE_PROPERTY_QUERY{StorageDeviceProperty, PropertyStandardQuery}
	out := make([]byte, 1024)
	n, err := ioctl(h, ioctlStorageQueryProperty, q, out)
	if err != nil || n < 36 {
		return descriptor{}, fmt.Errorf("storage query: %w", err)
	}
	out = out[:n]
	le := binary.LittleEndian
	vendor, product := cstr(out, le.Uint32(out[12:])), cstr(out, le.Uint32(out[16:]))
	model := strings.TrimSpace(vendor + " " + product)
	return descriptor{
		model:     model,
		serial:    cstr(out, le.Uint32(out[24:])),
		bus:       busNames[le.Uint32(out[28:])],
		removable: out[10] != 0,
	}, nil
}

func diskLength(h windows.Handle) int64 {
	out := make([]byte, 256) // DISK_GEOMETRY_EX: DISK_GEOMETRY (24 bytes) then DiskSize
	if n, err := ioctl(h, ioctlDiskGetGeometryEx, nil, out); err != nil || n < 32 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(out[24:]))
}

// List returns every volume with a drive letter (except empty card-reader slots).
func List() ([]Drive, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	sysDrive := strings.ToUpper(os.Getenv("SystemDrive"))
	var out []Drive
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		if d, ok := volume(letter, sysDrive); ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func volume(letter, sysDrive string) (Drive, bool) {
	root := letter + `:\`
	rootp, _ := windows.UTF16PtrFromString(root)
	t := windows.GetDriveType(rootp)
	if t == windows.DRIVE_NO_ROOT_DIR || t == windows.DRIVE_UNKNOWN {
		return Drive{}, false
	}
	d := Drive{Root: root, System: letter+":" == sysDrive, Removable: t == windows.DRIVE_REMOVABLE, Device: `\\.\` + letter + ":"}
	var label, fsName [261]uint16
	var serial, maxc, flags uint32
	err := windows.GetVolumeInformation(rootp, &label[0], 261, &serial, &maxc, &flags, &fsName[0], 261)
	switch {
	case errors.Is(err, windows.ERROR_NOT_READY) && t != windows.DRIVE_FIXED:
		return Drive{}, false // empty reader slot / no disc
	case errors.Is(err, errUnrecognizedVolume):
		d.RAW, d.FS = true, "RAW"
	case err != nil:
		d.Locked = true
	default:
		d.Label = windows.UTF16ToString(label[:])
		d.FS = windows.UTF16ToString(fsName[:])
		d.ID.FSSerial = fmt.Sprintf("%04X-%04X", serial>>16, serial&0xFFFF)
		d.WriteProtected = flags&fileReadOnlyVolume != 0
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(rootp, &free, &total, &totalFree) == nil {
			d.Total, d.Free = total, free
		}
	}
	d.ID.Label = d.Label
	if h, err := openQuery(`\\.\` + letter + ":"); err == nil {
		out := make([]byte, 12)
		if _, err := ioctl(h, ioctlStorageGetDeviceNumber, nil, out); err == nil {
			d.PhysicalID = fmt.Sprintf("PhysicalDrive%d", binary.LittleEndian.Uint32(out[4:]))
		}
		if _, err := ioctl(h, ioctlDiskIsWritable, nil, nil); errors.Is(err, windows.ERROR_WRITE_PROTECT) {
			d.WriteProtected = true
		}
		if desc, err := queryDescriptor(h); err == nil {
			d.ID.Model, d.ID.DeviceSerial = desc.model, desc.serial
			d.Removable = d.Removable || desc.removable || desc.bus == "sd" || desc.bus == "mmc"
		}
		windows.CloseHandle(h)
	}
	if d.PhysicalID != "" {
		if h, err := openQuery(`\\.\` + d.PhysicalID); err == nil {
			d.ID.Size = uint64(diskLength(h))
			windows.CloseHandle(h)
		}
	}
	d.ID.PhysicalID = d.PhysicalID
	return d, true
}

// volumeDisks maps each lettered volume to the physical disk numbers it spans.
func volumeDisks(letter string) []uint32 {
	h, err := openQuery(`\\.\` + letter + ":")
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	out := make([]byte, 8+24*8)
	n, err := ioctl(h, ioctlVolumeGetDiskExtents, nil, out)
	if err != nil || n < 8 {
		return nil
	}
	cnt := binary.LittleEndian.Uint32(out)
	var ds []uint32
	for i := uint32(0); i < cnt && int(8+24*i+4) <= n; i++ {
		ds = append(ds, binary.LittleEndian.Uint32(out[8+24*i:]))
	}
	return ds
}

// ListDevices returns physical disks 0..63, including ones with no mounted volume (RAW cards).
func ListDevices() ([]Device, error) {
	vols, err := List()
	if err != nil {
		return nil, err
	}
	byDisk := map[string][]Drive{}
	for _, v := range vols {
		for _, n := range volumeDisks(v.Root[:1]) {
			id := fmt.Sprintf("PhysicalDrive%d", n)
			byDisk[id] = append(byDisk[id], v)
		}
	}
	var out []Device
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("PhysicalDrive%d", i)
		h, err := openQuery(`\\.\` + id)
		if err != nil {
			continue
		}
		dev := Device{Path: `\\.\` + id, PhysicalID: id, Size: diskLength(h), Volumes: byDisk[id]}
		if desc, err := queryDescriptor(h); err == nil {
			dev.Model, dev.Serial, dev.Bus = desc.model, desc.serial, desc.bus
			dev.Removable = desc.removable || desc.bus == "sd" || desc.bus == "mmc"
		}
		if _, err := ioctl(h, ioctlDiskIsWritable, nil, nil); errors.Is(err, windows.ERROR_WRITE_PROTECT) {
			dev.ReadOnly = true
		}
		windows.CloseHandle(h)
		dev.Mounted = len(dev.Volumes) > 0
		out = append(out, dev)
	}
	return out, nil
}
