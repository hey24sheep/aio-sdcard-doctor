package job

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockSourceOS uses a named mutex: it exists for as long as any process holds a handle to it.
func lockSourceOS(hash string) (func(), error) {
	name, err := windows.UTF16PtrFromString(`Local\aio-sdcard-doctor-` + hash)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(h)
		return nil, ErrSourceBusy
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, ErrSourceBusy // held by an elevated run of this program
	}
	if err != nil {
		return nil, err
	}
	return func() { windows.CloseHandle(h) }, nil
}
