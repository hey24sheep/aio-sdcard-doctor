package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// M4 review #3: names from a damaged or crafted card (newlines, escape sequences, bell) can't
// break the screen or reach the terminal as commands.
func TestCardTextCannotBreakTheScreen(t *testing.T) {
	evil := drives.Drive{Root: `Q:\`, Label: "EVIL\nLINE\x1b]0;pwned\x07\x1b[2J", FS: "exFAT", Total: 1 << 30, Removable: true}
	r := newRig(t, evil)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			for i := 0; i < 30; i++ {
				env.Log("info", "DCIM/a\nb\rc\x1b]52;c;ZXZpbA==\x07.jpg")
			}
			return job.Result{Clean: true}, nil
		}
	}
	r.size(80, 24)
	check := func(where string) {
		t.Helper()
		sc := r.screen()
		lines := strings.Split(sc, "\n")
		if len(lines) > 24 {
			t.Fatalf("%s: %d lines on a 24-line screen", where, len(lines))
		}
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\r"} {
			if strings.Contains(sc, bad) {
				t.Fatalf("%s: %q reached the terminal", where, bad)
			}
		}
		for i, l := range lines {
			if lipgloss.Width(l) > 80 {
				t.Fatalf("%s: line %d too wide", where, i)
			}
		}
	}
	check("cards")
	r.key("enter")
	check("what to do")
	r.key("enter", "enter")
	check("done")
}

func TestStartOverKey(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		if !a.req.Fresh {
			a.plan.Issues = []job.Issue{{Kind: "resume", Title: "5 of 9 files already copied"}}
		}
	}
	r.key("enter", "enter")
	r.want(t, "[ ] Start over instead of continuing")
	r.key("o")
	r.want(t, "[x] Start over instead of continuing")
}
