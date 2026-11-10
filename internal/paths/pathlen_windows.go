package paths

import "unicode/utf16"

// windowsNames enables the Windows-only name rules (reserved characters and device names).
const windowsNames = true

// plen is the path length the OS limit is measured in: UTF-16 code units on Windows.
func plen(s string) int {
	n := 0
	for _, r := range s {
		n += runeUnits(r)
	}
	return n
}

func runeUnits(r rune) int {
	if utf16.IsSurrogate(r) || r < 0x10000 {
		return 1
	}
	return 2
}
