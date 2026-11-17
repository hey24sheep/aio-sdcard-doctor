package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on ANSI sequences in the Windows console (off by default in the old console
// host); false when it can't, and the look then stays without colour.
func enableVT() bool {
	ok := true
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err != nil {
			ok = false
			continue
		}
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
			if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
				ok = false
			}
		}
	}
	return ok
}
