package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// Adversarial pass U4: Ctrl+C opened "Stop now?", then the engine asked a question: the stop
// prompt stayed on screen over it while keys answered the hidden question blind.
func TestQuestionShownOverStopPrompt(t *testing.T) {
	r := newRig(t)
	ask := make(chan struct{})
	got := make(chan job.Choice, 1)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			<-ask
			got <- env.Prompt.Ask(ctx, job.Issue{Kind: "stalled", Title: "Card not responding", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
			return job.Result{Clean: true}, nil
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start
	r.m.Update(viewMsg{})
	r.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	r.want(t, "Stop now?")
	close(ask)
	for i := 0; i < 200 && r.m.v.Issue == nil; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	if sc := r.screen(); !strings.Contains(sc, "Card not responding") || strings.Contains(sc, "Stop now?") {
		t.Fatalf("the question is hidden:\n%s", sc)
	}
	r.m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if c := <-got; c != job.Stop {
		t.Fatalf("choice %s", c)
	}
	r.settle()
}

// Adversarial pass U5: at 80×24 a long check page pushed the "Problem:" line off the screen, so
// Enter seemed to do nothing. The problem line is always shown.
func TestProblemLineVisibleOnSmallTerminal(t *testing.T) {
	r := newRig(t)
	r.size(80, 24)
	r.mutate = func(a *fakeAction) {
		for i := 0; i < 30; i++ {
			a.plan.Summary = append(a.plan.Summary, "line of the summary")
		}
		a.plan.Issues = []job.Issue{{Kind: "x", Severity: job.Warn, Title: "A warning to accept"}}
	}
	r.key("enter", "enter") // the card, then copy everything: the check page
	r.key("enter")          // start without accepting the warning
	r.want(t, "Problem:")
}

// Adversarial pass (minor): invisible bidi marks in a card's file name could reorder what the
// terminal shows ("gpj.exe" displayed as "exe.jpg"). They are cleaned like control characters.
func TestBidiMarksCleaned(t *testing.T) {
	if got := cleanStr("photo\u202Egpj.exe"); strings.ContainsRune(got, '\u202E') {
		t.Fatalf("bidi override kept: %q", got)
	}
	if got := cleanStr("👨\u200d👩"); got != "👨\u200d👩" {
		t.Fatalf("emoji joiner removed: %q", got)
	}
}

// Adversarial pass (minor): a question started on its first choice, so Enter could answer "Try
// again" when the default (highlighted in the GUI) was "Stop". It starts on the default.
func TestQuestionStartsOnItsDefault(t *testing.T) {
	r := newRig(t)
	got := make(chan job.Choice, 1)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			got <- env.Prompt.Ask(ctx, job.Issue{Kind: "x", Title: "Retry?", Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Stop})
			return job.Result{Clean: true}, nil
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 200 && r.m.v.Issue == nil; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c := <-got; c != job.Stop {
		t.Fatalf("Enter answered %s, not the default", c)
	}
	r.settle()
}
