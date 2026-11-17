package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
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

func toPickList(t *testing.T, r *rig) {
	t.Helper()
	r.key("enter", "down", "down", "enter") // Find deleted files
	for _, c := range `E:\out` {
		r.key(string(c))
	}
	r.key("enter")
	for i := 0; i < 5; i++ { // Continue is the last option
		r.key("down")
	}
	r.key("enter", "enter") // continue, start the listing
	r.want(t, "Pick the files to bring back")
}

// Adversarial pass 2 SU3: a stray Esc, Left or Backspace on the pick list threw away a listing
// that may have taken hours. Leaving it is deliberate: n (start over), as the GUI's button.
func TestStrayBackKeepsThePickList(t *testing.T) {
	r := newRig(t)
	toPickList(t, r)
	r.key("esc", "backspace")
	r.m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	r.settle()
	if r.m.v.Step != app.StepRecoverList {
		t.Fatalf("a stray key left the pick list for %s", r.m.v.Step)
	}
	r.key("n", "y") // leaving is asked first (pass 3 OU4)
	if r.m.v.Step != app.StepDrive {
		t.Fatalf("n: %s", r.m.v.Step)
	}
}

// Adversarial pass 2 SU9: "I've read the warnings" stayed ticked when the check was made again
// on the same page (Start over) and showed different warnings.
func TestAcceptResetWhenWarningsChange(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		if a.req.Fresh {
			a.plan.Issues = []job.Issue{{Kind: "b", Severity: job.Warn, Title: "Every file is copied again"}}
		} else {
			a.plan.Issues = []job.Issue{{Kind: "resume", Title: "5 of 9 files already copied"}, {Kind: "a", Severity: job.Warn, Title: "Same disk"}}
		}
	}
	r.key("enter", "enter")
	r.key("a")
	r.want(t, "[x] I've read the warnings")
	r.key("o")
	r.want(t, "Every file is copied again", "[ ] I've read the warnings")
}

// Adversarial pass 2 (lead's scoring): "1 files" on the backup choices.
func TestOneFileIsSingular(t *testing.T) {
	m := &Model{w: 120, h: 40, v: app.View{Step: app.StepChoice, Choices: []app.ChoiceRow{{ID: "quick", Title: "Quick", Files: 1, Bytes: 10}}}}
	if b := strings.Join(m.body(m.pal()), "\n"); !strings.Contains(b, " 1 file · ") {
		t.Fatalf("choices:\n%s", b)
	}
}

// Adversarial pass 2 OU4: "Make an image instead" ended on "Stopped" with "Check the copy" as the
// next step. It says what happened and offers the image.
func TestImageInsteadSaysSo(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(context.Context, job.Env) (job.Result, error) { return job.Result{}, rescue.ErrImageInstead }
	}
	r.key("enter", "enter", "enter") // the card, copy everything, start
	r.want(t, "Stopped to make a full backup image instead", "Make a full backup image")
	if sc := r.screen(); strings.Contains(sc, "Check the copy") {
		t.Fatalf("offers to check a copy that was stopped:\n%s", sc)
	}
}

// Adversarial pass 3 OU14/SU11: Ctrl+C at a question did nothing; it is the safe answer, as Esc.
func TestCtrlCAtAQuestionIsTheSafeAnswer(t *testing.T) {
	r := newRig(t)
	got := make(chan job.Choice, 1)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			got <- env.Prompt.Ask(ctx, job.Issue{Kind: "stalled", Title: "Card not responding", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
			return job.Result{Clean: true}, nil
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start
	for i := 0; i < 200 && r.m.v.Issue == nil; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	r.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	select {
	case c := <-got:
		if c != job.Stop {
			t.Fatalf("Ctrl+C answered %s", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl+C at a question did nothing")
	}
	r.settle()
}

// Adversarial pass 3 OU4: one n or q on the pick list threw away a listing that may have taken
// hours. Both ask first.
func TestLeavingThePickListAsksFirst(t *testing.T) {
	for _, k := range []string{"n", "q"} {
		r := newRig(t)
		toPickList(t, r)
		r.key(k)
		if r.m.v.Step != app.StepRecoverList {
			t.Fatalf("%s left the list without asking (%s)", k, r.m.v.Step)
		}
		r.want(t, "Leave the list?")
		r.key("n") // no: stay
		if r.m.v.Step != app.StepRecoverList || strings.Contains(r.screen(), "Leave the list?") {
			t.Fatalf("%s then no: %s\n%s", k, r.m.v.Step, r.screen())
		}
		r.key(k)
		cmd := r.m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		r.settle()
		if k == "n" && r.m.v.Step != app.StepDrive {
			t.Fatalf("n then yes: %s", r.m.v.Step)
		}
		if k == "q" && (cmd == nil || cmd() != tea.Quit()) {
			t.Fatal("q then yes didn't quit")
		}
	}
}

// Adversarial pass 3 OU5: the image's destination asked for "a folder"; a typed folder ended in
// "file already exists" or an image named like a folder. It asks for the image file, as the GUI.
func TestImageDestinationAsksForAFile(t *testing.T) {
	r := newRig(t)
	r.key("enter") // the card
	for i, it := range r.m.actionList() {
		if !it.quick && it.row.Name == "image" {
			r.m.cur = i
		}
	}
	r.key("enter")
	if r.m.v.Step != app.StepDest {
		t.Fatalf("at %s", r.m.v.Step)
	}
	r.want(t, "Image file: ", "e.g. D:\\card.img")
	if sc := r.screen(); strings.Contains(sc, "Folder: ") {
		t.Fatalf("asks for a folder:\n%s", sc)
	}
}

// Adversarial pass 3 OU2: the stop question for a card tool said "the current block is finished
// and saved"; a repair or format can't stop half-way — the system tool finishes what it started.
func TestCardToolStopPromptSaysTheToolFinishes(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			<-ctx.Done()
			return job.Result{Clean: true}, nil // the tool finished its repair after Stop
		}
	}
	r.key("enter")
	for i := 0; i < 6; i++ {
		r.key("down") // to Card tools
	}
	r.key("enter", "space") // Check the file system
	r.key("down", "down", "down", "down", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start
	for i := 0; i < 200 && r.m.v.Step != app.StepRun; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	r.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if sc := r.screen(); !strings.Contains(sc, "finishes what it started") || strings.Contains(sc, "current block") {
		t.Fatalf("card tool stop prompt:\n%s", sc)
	}
	r.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	r.settle()
	if sc := r.screen(); !strings.Contains(sc, "Done") || strings.Contains(sc, "Stopped") {
		t.Fatalf("the tool finished, the screen says:\n%s", sc)
	}
}

// Adversarial pass 3 SU11: line and paragraph separators, invisible format marks (U+2060–206F)
// and tag characters (U+E0000–E007F) can reorder or hide text too.
func TestInvisibleMarksCleaned(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029, 0x2060, 0x2064, 0x206f, 0xe0001, 0xe0041, 0xe007f} {
		if got := cleanStr("a" + string(r) + "b"); got != "a b" {
			t.Errorf("U+%04X kept: %q", r, got)
		}
	}
}
