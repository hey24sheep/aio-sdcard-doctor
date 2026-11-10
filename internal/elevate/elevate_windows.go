package elevate

import (
	"context"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IsAdmin reports whether the process token is elevated.
func IsAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

// shellExecuteInfo is SHELLEXECUTEINFOW (natural alignment matches the C layout on 32/64-bit).
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x40
	seeMaskNoAsync        = 0x100
	swShowNormal          = 1
)

var procShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// Relaunch shows the UAC prompt to re-run the program with args as Administrator, waits for it
// and propagates its exit code. Cancelling the prompt returns ErrDeclined.
func Relaunch(ctx context.Context, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(childParams(args))
	cwd, _ := os.Getwd()
	dir, _ := windows.UTF16PtrFromString(cwd)
	info := shellExecuteInfo{fMask: seeMaskNoCloseProcess | seeMaskNoAsync, lpVerb: verb, lpFile: file,
		lpParameters: params, lpDirectory: dir, nShow: swShowNormal}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(e, windows.ERROR_CANCELLED) {
			return ErrDeclined
		}
		return e
	}
	defer windows.CloseHandle(info.hProcess)
	for { // bounded by ctx: poll so a cancelled run stops waiting
		ev, err := windows.WaitForSingleObject(info.hProcess, 500)
		if err != nil {
			return err
		}
		if ev == windows.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: int(code)}
	}
	return nil
}
