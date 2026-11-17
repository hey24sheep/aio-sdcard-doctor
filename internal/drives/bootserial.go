package drives

import (
	"encoding/binary"
	"fmt"
)

// bootSectorSerial reads the volume serial from a FAT12/16/32 or exFAT boot sector, formatted as
// Windows shows it ("1234-ABCD"); "" for anything else. macOS reports a UUID it derives for these
// file systems, which matches neither Windows nor a raw read of the same card (OA11).
func bootSectorSerial(b []byte) string {
	if len(b) < 512 || b[510] != 0x55 || b[511] != 0xAA {
		return ""
	}
	le := binary.LittleEndian
	var s uint32
	switch {
	case string(b[3:11]) == "EXFAT   ":
		s = le.Uint32(b[100:])
	case string(b[3:11]) == "NTFS    ", le.Uint16(b[11:]) == 0 || b[13] == 0: // no bytes per sector / cluster: not FAT
		return ""
	case le.Uint16(b[22:]) == 0 && b[66] == 0x29: // FAT32: sectors per FAT in the extended BPB
		s = le.Uint32(b[67:])
	case b[38] == 0x29: // FAT12/16 extended boot signature
		s = le.Uint32(b[39:])
	default:
		return ""
	}
	return fmt.Sprintf("%04X-%04X", s>>16, s&0xFFFF)
}
