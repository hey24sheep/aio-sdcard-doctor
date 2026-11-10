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
	return startedByDoubleClick(int(n))
}

func startedByDoubleClick(consoleProcesses int) bool { return consoleProcesses <= 1 }
