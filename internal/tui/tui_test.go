package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
)

// Made-up drives and a fake action: no test touches a real drive.
type fakeAction struct {
	req  app.Request
	plan job.Plan
	run  func(ctx context.Context, env job.Env) (job.Result, error)
	rows []recovery.Row
}

func (f *fakeAction) Name() string { return f.req.Action }
func (f *fakeAction) Plan(context.Context, job.Env) (job.Plan, error) {
	if len(f.plan.Summary) == 0 {
		f.plan.Summary = []string{"From: " + f.req.From + " — only read", "To: " + f.req.Dest}
	}
	return f.plan, nil
}
func (f *fakeAction) Run(ctx context.Context, env job.Env, _ job.Plan) (job.Result, error) {
	if f.run != nil {
		return f.run(ctx, env)
	}
	return job.Result{Counters: map[string]int{"OK": 7}, Clean: true}, nil
}
func (f *fakeAction) Rows() []recovery.Row          { return f.rows }
func (f *fakeAction) ConfirmText() (string, string) { return "CAMERA", "64.0 GB" }
func (f *fakeAction) Confirm(l, s string) error {
	if l != "CAMERA" || s != "64.0 GB" {
		return errors.New("doesn't match")
	}
	return nil
}

var (
	cardQ  = drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "exFAT", Total: 64 << 30, Free: 10 << 30, Removable: true}
	sysC   = drives.Drive{Root: `C:\`, Label: "Windows", FS: "NTFS", Total: 500 << 30, System: true}
	dataD  = drives.Drive{Root: `D:\`, Label: "Data", FS: "NTFS", Total: 200 << 30}
	locked = drives.Drive{Root: `L:\`, Label: "SECRET", FS: "exFAT", Total: 32 << 30, Removable: true, Locked: true}
)

type rig struct {
	m      *Model
	mu     sync.Mutex
	mutate func(*fakeAction)
	tools  []job.Capability
}

func newRig(t *testing.T, ds ...drives.Drive) *rig {
	t.Helper()
	if len(ds) == 0 {
		ds = []drives.Drive{sysC, dataD, cardQ}
	}
	r := &rig{}
	ctx, cancel := context.WithCancel(context.Background())
	s := app.NewSession(ctx, app.Deps{
		ListDrives: func() ([]drives.Drive, error) { return ds, nil },
		NewAction: func(req app.Request) (job.Action, error) {
			a := &fakeAction{req: req}
			if req.List {
				a.rows = []recovery.Row{{ID: "D:a.jpg", Path: "DCIM/a.jpg", Name: "a.jpg", State: recovery.Excellent, Size: 5000},
					{ID: "D:b.jpg", Path: "DCIM/b.jpg", Name: "b.jpg", State: recovery.Unrecoverable, Comment: "Overwritten by DCIM/c.jpg"}}
			}
			r.mu.Lock()
			if r.mutate != nil {
				r.mutate(a)
			}
			r.mu.Unlock()
			return a, nil
		},
		Choices: func(context.Context, string) ([]app.ChoiceRow, error) {
			return []app.ChoiceRow{{ID: "quick", Title: "Quick", Files: 3, Bytes: 1 << 20}, {ID: "full", Title: "Full", Files: 9, Bytes: 9 << 20}}, nil
		},
		IsAdmin:     func() bool { return true },
		DefaultDest: func(string) string { return `D:\rescue` },
		FixTools:    func(string) []job.Capability { r.mu.Lock(); defer r.mu.Unlock(); return r.tools },
	})
	t.Cleanup(func() { s.Close(); cancel() })
	r.m = New(s, Options{})
	r.size(120, 40)
	return r
}

func (r *rig) size(w, h int) { r.m.Update(tea.WindowSizeMsg{Width: w, Height: h}) }

// key sends keys: names like "enter", "down", "esc", "ctrl+c", or text typed rune by rune.
func (r *rig) key(names ...string) {
	for _, n := range names {
		var msg tea.KeyMsg
		switch n {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "ctrl+c":
			msg = tea.KeyMsg{Type: tea.KeyCtrlC}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(n)}
		}
		r.m.Update(msg)
		r.settle()
	}
}

// settle waits for background planning/running to finish and refreshes the view.
func (r *rig) settle() {
	for i := 0; i < 300; i++ {
		r.m.Update(viewMsg{})
		if r.m.v.Busy == "" && r.m.v.Step != app.StepRun {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) screen() string { return r.m.View() }

func (r *rig) want(t *testing.T, subs ...string) {
	t.Helper()
	sc := r.screen()
	for _, s := range subs {
		if !strings.Contains(sc, s) {
			t.Fatalf("screen lacks %q:\n%s", s, sc)
		}
	}
}

func TestPickerRemovableFirst(t *testing.T) {
	r := newRig(t)
	sc := r.screen()
	if !strings.Contains(sc, "CAMERA") || strings.Index(sc, "CAMERA") > strings.Index(sc, "Data") {
		t.Fatalf("card not first:\n%s", sc)
	}
	r.want(t, "Removable", "Nothing on the card is changed")
	r.key("enter")
	r.want(t, "Only read") // the header says so once a card is chosen
}

func TestPickerHidesSystemUntilS(t *testing.T) {
	r := newRig(t)
	if strings.Contains(r.screen(), "Windows") {
		t.Fatal("system drive shown")
	}
	r.want(t, "1 system drive hidden")
	r.key("s")
	r.want(t, "Windows", "System")
}

func TestLockedDriveHint(t *testing.T) {
	r := newRig(t, locked)
	r.key("enter")
	r.want(t, "Unlock the card first")
}

func TestQuickRescueKeys(t *testing.T) {
	r := newRig(t)
	r.key("enter") // the card
	r.want(t, "Copy everything off this card")
	r.key("enter") // quick path
	r.want(t, "Please check before it starts", `D:\rescue`)
	r.key("enter") // start
	r.want(t, "Done", "OK", "7", "Check the copy")
}

func TestDisabledOptionShowsReason(t *testing.T) {
	r := newRig(t)
	r.key("p")
	for _, c := range t.TempDir() {
		r.key(string(c))
	}
	r.key("enter")
	r.want(t, "Make a full backup image", "Needs the card itself, not a folder")
	// moving to it and pressing Enter shows the reason, not the next step
	for i := 0; i < 3; i++ {
		r.key("down")
	}
	r.key("enter")
	if r.m.v.Step != app.StepAction {
		t.Fatalf("disabled action chosen: %s", r.m.v.Step)
	}
}

func TestReviewBlocksOnBlockIssue(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Issues = []job.Issue{{Kind: "same-disk", Severity: job.Block, Title: "The destination is on the card's disk"}}
	}
	r.key("enter", "enter")
	r.want(t, "Can't start", "The destination is on the card's disk")
	r.key("enter")
	if r.m.v.Step != app.StepReview {
		t.Fatalf("started with a block issue: %s", r.m.v.Step)
	}
}

func TestWarningsNeedAccept(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Issues = []job.Issue{{Kind: "x", Severity: job.Warn, Title: "Same disk", Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue}}
	}
	r.key("enter", "enter")
	r.want(t, "Warning", "Same disk", "[ ] I've read the warnings")
	r.key("enter")
	if r.m.v.Step != app.StepReview {
		t.Fatal("started without accepting")
	}
	r.key("a")
	r.want(t, "[x] I've read the warnings")
	r.key("enter")
	r.want(t, "Done")
}

func TestIssueModalDeliversChoice(t *testing.T) {
	r := newRig(t)
	got := make(chan job.Choice, 1)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			got <- env.Prompt.Ask(ctx, job.Issue{Kind: "stalled", Title: "Card not responding", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
			return job.Result{Clean: true}, nil
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // start (don't settle: the run waits on the question)
	for i := 0; i < 200 && r.m.v.Issue == nil; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	r.want(t, "Card not responding", "Wait for the card", "Stop")
	r.m.Update(tea.KeyMsg{Type: tea.KeyRight})
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c := <-got; c != job.Stop {
		t.Fatalf("choice %s", c)
	}
	r.settle()
	r.want(t, "Done")
}

func TestCtrlCModal(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			env.Progress(job.Progress{Phase: "Copying files", Done: 10, Total: 100, Counters: map[string]int{"OK": 3}})
			<-ctx.Done()
			return job.Result{}, ctx.Err()
		}
	}
	r.key("enter", "enter")
	r.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for i := 0; i < 200 && r.m.v.Step != app.StepRun; i++ {
		time.Sleep(5 * time.Millisecond)
		r.m.Update(viewMsg{})
	}
	time.Sleep(20 * time.Millisecond)
	r.m.Update(viewMsg{})
	r.want(t, "3 files safe so far", "10%")
	r.m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	r.want(t, "Stop now?")
	r.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	r.settle()
	r.want(t, "Stopped")
}

func TestFixTypedConfirm(t *testing.T) {
	r := newRig(t)
	r.key("enter")
	for i := 0; i < 6; i++ {
		r.key("down") // to Card tools
	}
	r.key("enter")
	r.want(t, "Format the card")
	r.key("down", "down", "space") // format
	r.key("down", "down", "enter") // Continue
	r.want(t, "Type the card's label and size", "CAMERA", "64.0 GB")
	r.key("enter")
	if r.m.v.Step != app.StepReview {
		t.Fatal("format started without typing")
	}
	r.key("tab")
	for _, c := range "CAMERA" {
		r.key(string(c))
	}
	r.key("tab")
	for _, c := range "64.0 GB" {
		r.key(string(c))
	}
	r.key("enter")
	r.want(t, "Done")
}

func TestRecoverListTick(t *testing.T) {
	r := newRig(t)
	r.key("enter", "down", "down", "enter") // Find deleted files
	r.key("enter")                          // destination prefilled? type one
	if r.m.v.Step == app.StepDest {
		for _, c := range `E:\out` {
			r.key(string(c))
		}
		r.key("enter")
	}
	for r.m.v.Step == app.StepOptions {
		r.key("up", "up", "up", "enter")
		break
	}
	if r.m.v.Step == app.StepOptions { // Continue is the last item
		for i := 0; i < 5; i++ {
			r.key("down")
		}
		r.key("enter")
	}
	r.key("enter") // start the index
	r.want(t, "Pick the files to bring back", "Excellent", "Unrecoverable", "Overwritten by DCIM/c.jpg")
	r.key("down", "space") // the unrecoverable row: not tickable
	r.want(t, "can't be brought back")
	r.key("up", "space")
	r.want(t, "1 ticked")
	r.key("enter")
	r.want(t, "Please check before it starts")
}

func TestViewFitsEverySize(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 50}} {
		r := newRig(t)
		r.size(sz[0], sz[1])
		check := func(where string) {
			t.Helper()
			lines := strings.Split(r.screen(), "\n")
			if len(lines) > sz[1] {
				t.Errorf("%dx%d %s: %d lines", sz[0], sz[1], where, len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > sz[0] {
					t.Errorf("%dx%d %s line %d is %d wide: %q", sz[0], sz[1], where, i, w, l)
				}
			}
		}
		check("cards")
		r.key("enter")
		check("what to do")
		r.key("enter")
		check("check")
		r.key("enter")
		check("done")
	}
}

func TestSmallTerminalMessage(t *testing.T) {
	r := newRig(t)
	r.size(60, 20)
	r.want(t, "at least 80×24")
}

func TestASCIIMode(t *testing.T) {
	r := newRig(t)
	r.m.opt.ASCII = true
	for _, s := range []string{"✔", "▲", "✖", "›", "●"} {
		if strings.Contains(r.screen(), s) {
			t.Fatalf("non-ASCII %q in ASCII mode:\n%s", s, r.screen())
		}
	}
}

// Final review I7: a card tool this OS can't run is listed greyed with the reason; choosing it
// says why instead of selecting it.
func TestFixToolNotAvailableShowsReason(t *testing.T) {
	r := newRig(t)
	r.tools = []job.Capability{{Name: "Check", Enabled: true}, {Name: "Repair", Enabled: true},
		{Name: "Format", Reason: "mkfs.exfat not installed (exfatprogs)"}, {Name: "Health test", Enabled: true}}
	r.key("enter")
	for i := 0; i < 6; i++ {
		r.key("down") // to Card tools
	}
	r.key("enter")
	r.want(t, "Format the card (not available)", "mkfs.exfat not installed (exfatprogs)")
	r.key("down", "down", "space") // format
	if r.m.v.Req.FixOp != "" {
		t.Fatalf("chose %q", r.m.v.Req.FixOp)
	}
	r.want(t, "Format the card: mkfs.exfat not installed")
}
