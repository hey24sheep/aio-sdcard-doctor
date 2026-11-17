//go:build windows

package fix

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// toolAttr starts chkdsk or format without a console window of its own and in its own process
// group. The windowed build has no console, so Windows would open a new one, and closing that
// window kills chkdsk /f or format mid-write; Ctrl+C in our console would reach it too. Stop
// decides instead (stopTool).
func toolAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
}
