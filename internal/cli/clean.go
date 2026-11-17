package cli

import (
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
)

// cleanText makes card-controlled text (file names, volume labels, paths) safe to print: control
// characters, escape sequences and the invisible marks that reorder text become spaces, so a
// crafted name can't rewrite the terminal, set the clipboard or pass "photo.exe" off as
// "photo.jpg". The same rule as the terminal UI.
func cleanText(s string) string { return tui.CleanText(s) }

// cleanLines cleans each line of a message, keeping the program's own line breaks.
func cleanLines(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		ls[i] = cleanText(l)
	}
	return strings.Join(ls, "\n")
}
