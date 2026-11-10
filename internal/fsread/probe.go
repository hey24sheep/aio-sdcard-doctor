package fsread

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/diskfs/go-diskfs/partition"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/diskfs/go-diskfs/partition/mbr"
)

// Volume is one file system found on a device or image.
type Volume struct {
	Index        int
	Offset, Size int64
	FS           string // fat12 fat16 fat32 exfat ntfs ext4 iso9660 apfs hfs+ unknown
	Label        string
	Serial       string
}

const (
	maxVolumes = 64
	maxEBRs    = 128
	sector     = 512
)

var le = binary.LittleEndian

func readAt(r io.ReaderAt, off int64, n int) []byte {
	b := make([]byte, n)
	m, _ := r.ReadAt(b, off)
	return b[:m]
}

func has(b []byte, off int, sig string) bool {
	return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
}

// Probe finds the volumes on a device or image: partitions from an MBR (with extended
// partitions) or GPT, or one "superfloppy" volume when the card has no partition table.
func Probe(r io.ReaderAt, size int64) ([]Volume, error) {
	if size < sector {
		return nil, fmt.Errorf("fsread: device too small (%d bytes)", size)
	}
	boot := readAt(r, 0, sector)
	if fsBoot(boot) != "" { // a boot sector at 0: no partition table
		return []Volume{identify(r, 0, size)}, nil
	}
	var vols []Volume
	for _, p := range partitions(r, size) {
		if len(vols) == maxVolumes {
			break
		}
		v := identify(r, p[0], p[1])
		v.Index = len(vols)
		vols = append(vols, v)
	}
	if len(vols) == 0 {
		vols = []Volume{identify(r, 0, size)}
	}
	return vols, nil
}

// partitions returns [offset, size] in bytes of each partition, clamped to the device.
func partitions(r io.ReaderAt, size int64) [][2]int64 {
	var out [][2]int64
	add := func(startSec, nSec uint64) {
		off, n := int64(startSec)*sector, int64(nSec)*sector
		if startSec == 0 || nSec == 0 || off >= size {
			return
		}
		out = append(out, [2]int64{off, min(n, size-off)})
	}
	tbl, err := partition.Read(newStorage(r, size), sector, sector)
	if err != nil {
		return nil
	}
	switch t := tbl.(type) {
	case *gpt.Table:
		for _, p := range t.Partitions {
			if p.Type != gpt.Unused && p.End >= p.Start {
				add(p.Start, p.End-p.Start+1)
			}
		}
	case *mbr.Table:
		for _, p := range t.Partitions {
			switch p.Type {
			case mbr.Empty:
			case 0x05, 0x0F, 0x85: // extended: walk the EBR chain
				for _, l := range logicals(r, uint64(p.Start)) {
					add(l[0], l[1])
				}
			default:
				add(uint64(p.Start), uint64(p.Size))
			}
		}
	}
	return out
}

// logicals walks an extended partition's EBR chain (bounded, loop-safe).
func logicals(r io.ReaderAt, base uint64) [][2]uint64 {
	var out [][2]uint64
	seen := map[uint64]bool{}
	cur := base
	for i := 0; i < maxEBRs && !seen[cur]; i++ {
		seen[cur] = true
		ebr := readAt(r, int64(cur)*sector, sector)
		if len(ebr) < sector || ebr[510] != 0x55 || ebr[511] != 0xAA {
			break
		}
		e1, e2 := ebr[446:462], ebr[462:478]
		if e1[4] != 0 {
			out = append(out, [2]uint64{cur + uint64(le.Uint32(e1[8:])), uint64(le.Uint32(e1[12:]))})
		}
		if e2[4] == 0 || le.Uint32(e2[8:]) == 0 {
			break
		}
		cur = base + uint64(le.Uint32(e2[8:]))
	}
	return out
}

// fsBoot names the file system whose boot sector this is ("" if none).
func fsBoot(b []byte) string {
	switch {
	case has(b, 3, "EXFAT   "):
		return "exfat"
	case has(b, 3, "NTFS    "):
		return "ntfs"
	}
	return fatKind(b)
}

// fatKind validates a FAT BPB and returns fat12/fat16/fat32 by cluster count (spec rule).
func fatKind(b []byte) string {
	if len(b) < sector || b[510] != 0x55 || b[511] != 0xAA || (b[0] != 0xEB && b[0] != 0xE9) {
		return ""
	}
	bps := int64(le.Uint16(b[11:]))
	spc, rsvd, nfats := int64(b[13]), int64(le.Uint16(b[14:])), int64(b[16])
	if bps != 512 && bps != 1024 && bps != 2048 && bps != 4096 || spc == 0 || spc&(spc-1) != 0 || nfats < 1 || nfats > 2 || rsvd == 0 {
		return ""
	}
	rootEnt := int64(le.Uint16(b[17:]))
	tot := int64(le.Uint16(b[19:]))
	if tot == 0 {
		tot = int64(le.Uint32(b[32:]))
	}
	fatsz := int64(le.Uint16(b[22:]))
	if fatsz == 0 {
		fatsz = int64(le.Uint32(b[36:]))
	}
	rootSecs := (rootEnt*32 + bps - 1) / bps
	data := tot - (rsvd + nfats*fatsz + rootSecs)
	if data <= 0 || fatsz == 0 {
		return ""
	}
	switch clusters := data / spc; {
	case clusters < 4085:
		return "fat12"
	case clusters < 65525:
		return "fat16"
	}
	return "fat32"
}

func trimLabel(b []byte) string {
	s := strings.TrimRight(string(b), " \x00")
	if s == "NO NAME" {
		return ""
	}
	return s
}

// identify reads the volume's signatures, label and serial.
func identify(r io.ReaderAt, off, size int64) Volume {
	v := Volume{Offset: off, Size: size, FS: "unknown"}
	head := readAt(r, off, 34*1024)
	boot := head
	if len(boot) > sector {
		boot = boot[:sector]
	}
	switch k := fsBoot(boot); {
	case k == "exfat" && len(boot) >= 104:
		v.FS, v.Serial = k, serial32(boot[100:])
	case k == "ntfs" && len(boot) >= 80:
		v.FS, v.Serial = k, fmt.Sprintf("%016X", le.Uint64(boot[72:]))
	case k == "fat32":
		v.FS, v.Serial, v.Label = k, serial32(boot[67:]), trimLabel(boot[71:82])
	case k == "fat12" || k == "fat16":
		v.FS, v.Serial, v.Label = k, serial32(boot[39:]), trimLabel(boot[43:54])
	case len(head) >= 1082 && le.Uint16(head[1080:]) == 0xEF53:
		v.FS = "ext4"
		if len(head) >= 1024+136 {
			u := head[1024+104 : 1024+120]
			v.Serial = fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
			v.Label = trimLabel(head[1024+120 : 1024+136])
		}
	case has(head, 32769, "CD001"):
		v.FS = "iso9660"
		if len(head) >= 32768+72 {
			v.Label = trimLabel(head[32768+40 : 32768+72])
		}
	case has(head, 32, "NXSB"):
		v.FS = "apfs"
	case has(head, 1024, "H+"), has(head, 1024, "HX"):
		v.FS = "hfs+"
	}
	return v
}

func serial32(b []byte) string {
	s := le.Uint32(b)
	return fmt.Sprintf("%04X-%04X", s>>16, s&0xFFFF)
}
