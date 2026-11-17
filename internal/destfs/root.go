package destfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// CheckRoot says, as an error, why dest can't be a destination folder: the write guard would
// otherwise panic on the first write (a pasted path with its quotes, a Windows device name such
// as "aux", a ':' inside a folder name, a device path).
func CheckRoot(dest string) (err error) {
	bad := func(why string) error {
		return fmt.Errorf("“%s” can't be used as the destination folder: %s", dest, why) // %q would double every backslash
	}
	if strings.TrimSpace(dest) == "" {
		return errors.New("choose a destination folder")
	}
	if strings.ContainsRune(dest, '"') {
		return bad("remove the quotes around it")
	}
	if isDevice(dest) {
		return bad("it is a device, not a folder")
	}
	if runtime.GOOS == "windows" {
		rest := dest[len(filepath.VolumeName(dest)):]
		if strings.ContainsAny(rest, `<>:|?*`) {
			return bad("a folder name can't contain < > : | ? or *")
		}
		for _, seg := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
			if winDevice(seg) {
				return bad(fmt.Sprintf("“%s” is a name Windows keeps for a device", seg))
			}
		}
	}
	defer func() {
		if p := recover(); p != nil {
			err = bad("it doesn't resolve to a folder of its own")
		}
	}()
	New(dest).(*guarded).check(filepath.Join(dest, "Reports"))
	return nil
}

// winDevice reports a Windows device name (CON, AUX, NUL, COM1 …), with or without an extension.
func winDevice(seg string) bool {
	s := []rune(strings.ToUpper(strings.TrimRight(strings.SplitN(seg, ".", 2)[0], " ")))
	switch string(s) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(s) == 4 && (string(s[:3]) == "COM" || string(s[:3]) == "LPT") {
		return strings.ContainsRune("123456789¹²³", s[3])
	}
	return false
}
