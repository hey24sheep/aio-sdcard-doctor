package cli

import (
	"context"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
)

func TestLaunchModeChoice(t *testing.T) {
	for _, c := range []struct {
		double, tty, plain bool
		want               string
	}{
		{true, true, false, "gui"},   // double-clicked: we own a fresh console
		{true, false, false, "gui"},  //
		{false, true, false, "tui"},  // typed in a terminal
		{false, true, true, "usage"}, // --plain (screen readers)
		{false, false, false, "usage"},
	} {
		if got := launchMode(c.double, c.tty, c.plain); got != c.want {
			t.Errorf("double=%v tty=%v plain=%v: %s, want %s", c.double, c.tty, c.plain, got, c.want)
		}
	}
}

func TestNoCommandOpensChosenFrontEnd(t *testing.T) {
	oldTUI, oldGUI, oldDouble := runTUI, runGUI, doubleClicked
	defer func() { runTUI, runGUI, doubleClicked = oldTUI, oldGUI, oldDouble }()
	var ran string
	runTUI = func(IO, tui.Options) error { ran = "tui"; return nil }
	runGUI = func(context.Context, IO, bool, int) error { ran = "gui"; return nil }
	doubleClicked = func() bool { return false }
	if code, _, _ := cli(t, true, ""); code != 0 || ran != "tui" {
		t.Fatalf("terminal: exit %d ran %q", code, ran)
	}
	ran = ""
	if code, out, _ := cli(t, false, ""); code != 0 || ran != "" || !contains(out, "Usage") {
		t.Fatalf("script: exit %d ran %q", code, ran)
	}
	doubleClicked = func() bool { return true }
	if code, _, _ := cli(t, false, ""); code != 0 || ran != "gui" {
		t.Fatalf("double-click: exit %d ran %q", code, ran)
	}
	ran = ""
	if code, _, _ := cli(t, false, "", "tui", "--ascii"); code != 0 || ran != "tui" {
		t.Fatalf("tui command: exit %d ran %q", code, ran)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
