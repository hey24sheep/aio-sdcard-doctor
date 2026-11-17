package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// testLook is a person's terminal w columns wide, without colour (the words and symbols only).
func testLook(w int, ascii bool) *Look {
	return &Look{VT: true, ASCII: ascii, Width: func() int { return w }, r: lipgloss.NewRenderer(&bytes.Buffer{})}
}

// The live line never wraps (it is redrawn in place), and keeps the files done when narrow.
func TestProgressLineFitsTheTerminal(t *testing.T) {
	pr := job.Progress{Phase: "Copying files", Item: "DCIM/100CANON/a-very-long-folder-name/IMG_4821.CR3", Done: 26 << 30, Total: 40 << 30,
		Items: 1301, ItemsTotal: 2031, BPS: 29.6e6}
	for _, w := range []int{40, 60, 80, 120} {
		for _, ascii := range []bool{false, true} {
			l := sty{testLook(w, ascii)}.progressLine(pr, w)
			if lipgloss.Width(l) > w-1 || !strings.Contains(l, "65%") || !strings.Contains(l, "1,301 of 2,031 files") {
				t.Errorf("%d columns: %q", w, l)
			}
		}
	}
	if l := (sty{testLook(120, false)}).progressLine(pr, 120); !strings.Contains(l, "about 9 min left") || !strings.Contains(l, "IMG_4821.CR3") {
		t.Errorf("wide: %q", l)
	}
}

// Scripts, pipes and --plain keep the plain lines; a person's terminal gets the verdict and the
// status symbols, with the words unchanged.
func TestResultPlainAndOnATerminal(t *testing.T) {
	res := job.Result{Counters: map[string]int{"OK": 2010, "FAILED": 9, "PARTIAL": 12}, Reports: []string{`D:\r\Reports\copy.csv`}}
	var plain bytes.Buffer
	printResult(IO{Out: &plain}, res)
	if want := "\nResult:\n  FAILED                        9\n  OK                         2010\n  PARTIAL                      12\nReport: D:\\r\\Reports\\copy.csv\n"; plain.String() != want {
		t.Fatalf("plain result changed:\n%q", plain.String())
	}
	var tty bytes.Buffer
	printResultOf(IO{Out: &tty, Look: testLook(80, false)}, res, false)
	o := tty.String()
	if !strings.Contains(o, "▲ Done, with problems") || strings.Index(o, "✔ OK") > strings.Index(o, "✖ FAILED") || !strings.Contains(o, "2,010") {
		t.Fatalf("terminal result:\n%s", o)
	}
	tty.Reset()
	printResultOf(IO{Out: &tty, Look: testLook(80, true)}, res, true)
	if o := tty.String(); !strings.Contains(o, "x Stopped") || !strings.Contains(o, "+ OK") {
		t.Fatalf("ASCII stopped result:\n%s", o)
	}
}

func TestPlanKeysLineUp(t *testing.T) {
	ls := sty{testLook(80, false)}.kvLines([]string{"From: Q:\\", "Options: x", "Warning: 1 folder could not be listed", "Left out by your choice: 3 files"})
	if ls[0] != "  From:    Q:\\" || ls[1] != "  Options: x" || ls[2] != "  ▲ Warning: 1 folder could not be listed" || ls[3] != "  Left out by your choice: 3 files" {
		t.Fatalf("%q", ls)
	}
}

func TestExitMessageKeepsItsWords(t *testing.T) {
	s := sty{testLook(80, false)}
	if got := s.exitMessage(ExitError, "Can't start: x\n  y"); got != "✖ Can't start: x\n  y" {
		t.Errorf("%q", got)
	}
	if got := s.exitMessage(ExitAdmin, "This needs administrator rights."); got != "▲ This needs administrator rights." {
		t.Errorf("%q", got)
	}
	if got := (sty{}).exitMessage(ExitError, "Can't start: x"); got != "Can't start: x" {
		t.Errorf("plain: %q", got)
	}
}

func TestPlainFlagTurnsTheLookOff(t *testing.T) {
	l := testLook(80, false)
	var out bytes.Buffer
	Main([]string{"--plain", "scan"}, IO{In: strings.NewReader(""), Out: &out, Err: &out, Look: l})
	if !l.off || styleOf(IO{Look: l}).on() {
		t.Fatal("--plain left the look on")
	}
}

// The scan summary saved to disk stays plain; only the terminal copy gets bold headings.
func TestReportStylingKeepsTheText(t *testing.T) {
	text := "Card scan  2026-11-17\nBy category:\n  Photos  1 file\n\nNext step: aio-sdcard-doctor rescue --from Q:\\\n"
	if got := (sty{}).report(text); got != text {
		t.Fatalf("plain changed: %q", got)
	}
	if got := (sty{testLook(80, false)}).report(text); got != text { // no colour: the same text
		t.Fatalf("no-colour terminal changed: %q", got)
	}
}
