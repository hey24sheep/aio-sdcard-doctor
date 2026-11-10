//go:build !windows

package paths

import "unicode/utf8"

// windowsNames enables the Windows-only name rules (reserved characters and device names).
const windowsNames = false

// plen is the path length the OS limit is measured in: bytes outside Windows.
func plen(s string) int { return len(s) }

func runeUnits(r rune) int {
	if n := utf8.RuneLen(r); n > 0 {
		return n
	}
	return 3 // utf8.RuneError is written as 3 bytes
}
