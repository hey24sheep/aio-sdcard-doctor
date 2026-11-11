package drives

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// volumeNameDOS asks GetFinalPathNameByHandle for a drive-letter path (VOLUME_NAME_DOS = 0).
const volumeNameDOS = 0

// openForQuery opens a file or folder only to ask about it (no read or write access).
func openForQuery(p string) (windows.Handle, bool) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, false
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, false
	}
	return h, true
}

// finalPath asks Windows where p really is (junctions, mount points and subst resolved).
func finalPath(p string) (string, bool) {
	h, ok := openForQuery(p)
	if !ok {
		return "", false
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS)
	if err != nil || int(n) >= len(buf) {
		return "", false
	}
	s := windows.UTF16ToString(buf[:n])
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		s = `\\` + s[len(`\\?\UNC\`):]
	case strings.HasPrefix(s, `\\?\`):
		s = s[len(`\\?\`):]
	}
	return s, true
}

// volumeSerial reads the serial of the volume holding p from an open handle: right even when p
// is reached through a junction or a folder mount point.
func volumeSerial(p string) (string, bool) {
	h, ok := openForQuery(p)
	if !ok {
		return "", false
	}
	defer windows.CloseHandle(h)
	var serial uint32
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, &serial, nil, nil, nil, 0); err != nil {
		return "", false
	}
	return fmt.Sprintf("%08X", serial), true
}
