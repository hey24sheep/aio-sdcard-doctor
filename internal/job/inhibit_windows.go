package job

import (
	"errors"
	"runtime"

	"golang.org/x/sys/windows"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// inhibitOS pins a goroutine to one OS thread for the run's lifetime, because
// SetThreadExecutionState is per thread (R21).
func inhibitOS(string) (func(), error) {
	if err := procSetThreadExecutionState.Find(); err != nil {
		return nil, err
	}
	started := make(chan error, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if r, _, _ := procSetThreadExecutionState.Call(esContinuous | esSystemRequired); r == 0 {
			started <- errors.New("SetThreadExecutionState failed")
			return
		}
		started <- nil
		<-stop
		procSetThreadExecutionState.Call(esContinuous)
	}()
	if err := <-started; err != nil {
		<-done
		return nil, err
	}
	return func() { close(stop); <-done }, nil
}
