//go:build windows

package cli

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procConsoleList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// isDoubleClicked: started from Explorer, the program owns a fresh console with no other process
// in it (a terminal would have its shell attached too) — or has no console at all (the windowed
// aio-sdcard-doctor-gui.exe build).
func isDoubleClicked() bool {
	var ids [4]uint32
	n, _, _ := procConsoleList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return startedByDoubleClick(int(n), stdoutKind())
}

// stdoutKind is where this process's standard output goes.
func stdoutKind() outKind {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return outNone
	}
	t, _ := windows.GetFileType(h)
	switch t {
	case windows.FILE_TYPE_PIPE:
		return outPipe
	case windows.FILE_TYPE_DISK:
		return outFile
	case windows.FILE_TYPE_CHAR:
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			return outConsole
		}
	}
	return outOther
}
