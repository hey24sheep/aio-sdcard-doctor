//go:build windows

package cli

import "testing"

// The windowed build (aio-sdcard-doctor-gui.exe, -H windowsgui) has no console at all: that is a
// double-click too, and must open the GUI rather than print usage to nowhere.
func TestStartedByDoubleClick(t *testing.T) {
	for n, want := range map[int]bool{0: true, 1: true, 2: false, 3: false} {
		if got := startedByDoubleClick(n); got != want {
			t.Errorf("%d console processes: %v, want %v", n, got, want)
		}
	}
}
