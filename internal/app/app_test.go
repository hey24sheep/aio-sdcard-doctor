package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/recovery"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fakeAction is a job.Action whose plan and run the test controls.
type fakeAction struct {
	req     Request
	plan    job.Plan
	run     func(ctx context.Context, env job.Env) (job.Result, error)
	rows    []recovery.Row
	confirm func(label, size string) error
	slow    bool // Plan waits until its context ends (a slow card)
}

func (f *fakeAction) Name() string { return f.req.Action }
func (f *fakeAction) Plan(ctx context.Context, _ job.Env) (job.Plan, error) {
	if f.slow {
		<-ctx.Done()
		return job.Plan{}, ctx.Err()
	}
	if len(f.plan.Summary) == 0 {
		f.plan.Summary = []string{"From: " + f.req.From}
	}
	return f.plan, nil
}
func (f *fakeAction) Run(ctx context.Context, env job.Env, _ job.Plan) (job.Result, error) {
	if f.run != nil {
		return f.run(ctx, env)
	}
	return job.Result{Counters: map[string]int{"OK": 3}, Clean: true}, nil
}
func (f *fakeAction) Rows() []recovery.Row { return f.rows }
func (f *fakeAction) ConfirmText() (string, string) {
	return "CAMERA", "64.0 GB"
}
func (f *fakeAction) Confirm(label, size string) error {
	if f.confirm != nil {
		return f.confirm(label, size)
	}
	if label != "CAMERA" || size != "64.0 GB" {
		return errors.New("doesn't match")
	}
	return nil
}

type rig struct {
	s      *Session
	mu     sync.Mutex
	made   []*fakeAction
	mutate func(*fakeAction)
	cancel context.CancelFunc
	admin  bool
	tools  []job.Capability // what fix.Available would say (nil: not known)
}

var card = drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "exFAT", Total: 64 << 30, Removable: true, PhysicalID: "MOCK7", Device: `\\.\Q:`}

func newRig(t *testing.T, ds ...drives.Drive) *rig {
	t.Helper()
	if len(ds) == 0 {
		ds = []drives.Drive{{Root: `C:\`, Label: "OS", FS: "NTFS", System: true, PhysicalID: "MOCK0"}, card}
	}
	r := &rig{admin: true}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.s = NewSession(ctx, Deps{
		ListDrives: func() ([]drives.Drive, error) { return ds, nil },
		NewAction: func(req Request) (job.Action, error) {
			a := &fakeAction{req: req}
			if req.List {
				a.rows = []recovery.Row{{ID: "D:a.jpg", Path: "a.jpg", Name: "a.jpg", State: recovery.Excellent}}
			}
			r.mu.Lock()
			if r.mutate != nil {
				r.mutate(a)
			}
			r.made = append(r.made, a)
			r.mu.Unlock()
			return a, nil
		},
		Choices: func(context.Context, string) ([]ChoiceRow, error) {
			return []ChoiceRow{{ID: "quick", Title: "Quick", Files: 3}, {ID: "full", Title: "Full", Files: 9}}, nil
		},
		IsAdmin:     func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.admin },
		Instruction: func([]string) string { return "Run as administrator: aio-sdcard-doctor gui" },
		DefaultDest: func(string) string { return `D:\aio-sdcard-doctor-CAMERA` },
		FixTools:    func(string) []job.Capability { r.mu.Lock(); defer r.mu.Unlock(); return r.tools },
	})
	t.Cleanup(func() { r.s.Close(); cancel() })
	return r
}

func (r *rig) do(t *testing.T, name string, kv ...string) {
	t.Helper()
	args := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[kv[i]] = kv[i+1]
	}
	if err := r.s.Do(Command{Name: name, Args: args}); err != nil {
		t.Fatalf("%s %v: %v", name, kv, err)
	}
}

// waitStep waits for the session to reach a step (planning and running happen in the background).
func (r *rig) waitStep(t *testing.T, step Step) View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := r.s.View(); v.Step == step && v.Busy == "" {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never reached %s (at %s, busy %q, error %q)", step, r.s.View().Step, r.s.View().Busy, r.s.View().Error)
	return View{}
}

func (r *rig) last() *fakeAction {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made[len(r.made)-1]
}

func TestQuickRescueThreeSteps(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepResults)
	req := r.last().req
	if req.Action != "rescue" || req.Backup != "full" || req.Compress || req.Dest != `D:\aio-sdcard-doctor-CAMERA` || req.From != card.Root {
		t.Fatalf("quick rescue request %+v", req)
	}
	if v.Result == nil || v.Result.Counters["OK"] != 3 {
		t.Fatalf("result %+v", v.Result)
	}
}

func TestCustomiseRevealsSteps(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "rescue")
	v := r.waitStep(t, StepChoice)
	if len(v.Choices) != 2 || v.Choices[1].Files != 9 {
		t.Fatalf("choices %+v", v.Choices)
	}
	r.do(t, "set-choice", "id", "quick")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\out`)
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "compress", "value", "1")
	r.do(t, "next")
	r.waitStep(t, StepReview)
	if req := r.last().req; req.Backup != "quick" || req.Dest != `E:\out` || !req.Compress {
		t.Fatalf("request %+v", req)
	}
}

func TestBackNavigationKeepsChoices(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "rescue")
	r.waitStep(t, StepChoice)
	r.do(t, "set-choice", "id", "quick")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\out`)
	r.waitStep(t, StepOptions)
	r.do(t, "back")
	v := r.waitStep(t, StepDest)
	if v.Req.Dest != `E:\out` || v.Req.Backup != "quick" {
		t.Fatalf("back lost choices: %+v", v.Req)
	}
}

func TestNextStepSuggestions(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepResults)
	if !slices.ContainsFunc(v.Next, func(n NextStep) bool { return n.Action == "verify" }) {
		t.Fatalf("next %+v", v.Next)
	}
}

func TestFlowEveryActionReachesResults(t *testing.T) {
	for _, act := range []string{"scan", "rescue", "image", "verify", "recover", "fix"} {
		t.Run(act, func(t *testing.T) {
			r := newRig(t)
			r.do(t, "select-drive", "root", card.Root)
			r.do(t, "choose-action", "action", act)
			for i := 0; i < 400; i++ {
				v := r.s.View()
				for v.Busy != "" || v.Step == StepRun {
					time.Sleep(5 * time.Millisecond)
					v = r.s.View()
				}
				switch v.Step {
				case StepChoice:
					r.do(t, "set-choice", "id", "full")
				case StepDest:
					r.do(t, "set-dest", "path", `E:\out`)
				case StepOptions:
					if act == "fix" {
						r.do(t, "set-option", "name", "fix-op", "value", "check")
					}
					if act == "verify" {
						r.do(t, "set-option", "name", "manifest", "value", `E:\out\Reports\manifest.json`)
					}
					r.do(t, "next")
				case StepReview:
					r.do(t, "start")
				case StepRecoverList:
					r.do(t, "recover-tick-all")
					r.do(t, "recover-start")
				case StepResults:
					return
				}
			}
			t.Fatalf("stuck at %s (%q)", r.s.View().Step, r.s.View().Error)
		})
	}
}

func TestDisabledActionNotSelectable(t *testing.T) {
	locked := card
	locked.Locked = true
	r := newRig(t, locked)
	r.do(t, "select-drive", "root", locked.Root)
	v := r.s.View()
	for _, a := range v.Actions {
		if a.Enabled || !strings.Contains(a.Reason, "nlock") {
			t.Fatalf("locked card action %+v", a)
		}
	}
	if err := r.s.Do(Command{Name: "choose-action", Args: map[string]string{"action": "rescue"}}); err == nil {
		t.Fatal("disabled action chosen")
	}
}

func TestDrivesRemovableFirstSystemHidden(t *testing.T) {
	r := newRig(t)
	v := r.s.View()
	if len(v.Drives) != 1 || v.Drives[0].Root != card.Root || v.HiddenSystem != 1 {
		t.Fatalf("drives %+v hidden %d", v.Drives, v.HiddenSystem)
	}
	r.do(t, "show-system")
	if v := r.s.View(); len(v.Drives) != 2 || v.Drives[0].Root != card.Root {
		t.Fatalf("with system: %+v", v.Drives)
	}
}

func TestReviewBlocksOnBlockIssue(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Issues = []job.Issue{{Kind: "same-disk", Severity: job.Block, Title: "Same disk"}}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	if err := r.s.Do(Command{Name: "start"}); err == nil {
		t.Fatal("started with a Block issue")
	}
}

func TestWarningsNeedAcceptance(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Issues = []job.Issue{{Kind: "same-disk-internal", Severity: job.Warn, Title: "Same disk", Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue}}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	if err := r.s.Do(Command{Name: "start"}); err == nil {
		t.Fatal("started without accepting the warning")
	}
	r.do(t, "start", "accept", "1")
	r.waitStep(t, StepResults)
}

func TestPromptRoundTrip(t *testing.T) {
	r := newRig(t)
	got := make(chan job.Choice, 1)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			got <- env.Prompt.Ask(ctx, job.Issue{Kind: "stalled", Title: "Card not responding", Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait})
			return job.Result{Clean: true}, nil
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	var id string
	for ev := range r.s.Events() {
		if ev.Kind == "issue" {
			id = ev.IssueID
			break
		}
	}
	// a renderer that reconnects sees the question still pending
	if v := r.s.View(); v.Issue == nil || v.Issue.ID != id || v.Issue.Issue.Title != "Card not responding" {
		t.Fatalf("pending issue %+v", v.Issue)
	}
	r.do(t, "answer", "id", id, "choice", string(job.Stop))
	if c := <-got; c != job.Stop {
		t.Fatalf("choice %s", c)
	}
	r.waitStep(t, StepResults)
}

func TestProgressCoalescingNeverBlocks(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			for i := 0; i < 200_000; i++ {
				env.Progress(job.Progress{Done: int64(i), Total: 200_000})
			}
			return job.Result{Clean: true}, nil
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepResults) // nobody reads Events(): the engine still finishes
	if v.Progress == nil || v.Progress.Done != 199_999 {
		t.Fatalf("latest progress %+v", v.Progress)
	}
}

func TestCancelStopsCleanly(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			<-ctx.Done()
			return job.Result{}, ctx.Err()
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.waitStep(t, StepRun)
	r.do(t, "cancel")
	v := r.waitStep(t, StepResults)
	if !strings.Contains(v.Error, "Stopped") {
		t.Fatalf("error %q", v.Error)
	}
}

func TestRecoverListFilterTick(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		if a.req.List {
			a.rows = []recovery.Row{
				{ID: "D:DCIM/a.jpg", Path: "DCIM/a.jpg", Name: "a.jpg", State: recovery.Excellent},
				{ID: "D:DCIM/b.mp4", Path: "DCIM/b.mp4", Name: "b.mp4", State: recovery.Poor},
				{ID: "D:x.jpg", Path: "x.jpg", Name: "x.jpg", State: recovery.Unrecoverable, Comment: "Overwritten"},
			}
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "recover")
	for r.s.View().Step != StepReview {
		v := r.s.View()
		switch {
		case v.Busy != "":
			time.Sleep(5 * time.Millisecond)
		case v.Step == StepDest:
			r.do(t, "set-dest", "path", `E:\out`)
		case v.Step == StepOptions:
			r.do(t, "next")
		}
	}
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepRecoverList)
	if v.Recover == nil || len(v.Recover.Rows) != 3 {
		t.Fatalf("list %+v", v.Recover)
	}
	r.do(t, "recover-filter", "text", "dcim")
	if v := r.s.View(); len(v.Recover.Rows) != 2 {
		t.Fatalf("filtered %+v", v.Recover.Rows)
	}
	r.do(t, "recover-tick", "id", "D:DCIM/a.jpg")
	if err := r.s.Do(Command{Name: "recover-tick", Args: map[string]string{"id": "D:x.jpg"}}); err == nil {
		t.Fatal("an unrecoverable row was ticked")
	}
	r.do(t, "recover-start")
	r.waitStep(t, StepReview)
	if req := r.last().req; req.List || !slices.Equal(req.Select, []string{"D:DCIM/a.jpg"}) {
		t.Fatalf("recover request %+v", req)
	}
}

func TestFixNeedsTypedConfirm(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "fix-op", "value", "format")
	r.do(t, "next")
	v := r.waitStep(t, StepReview)
	if v.Confirm == nil || v.Confirm.Label != "CAMERA" || v.Confirm.Size != "64.0 GB" {
		t.Fatalf("confirm %+v", v.Confirm)
	}
	if err := r.s.Do(Command{Name: "start"}); err == nil {
		t.Fatal("format started without typing label and size")
	}
	if err := r.s.Do(Command{Name: "start", Args: map[string]string{"label": "camera", "size": "64.0 GB"}}); err == nil {
		t.Fatal("wrong label accepted")
	}
	r.do(t, "start", "label", "CAMERA", "size", "64.0 GB")
	r.waitStep(t, StepResults)
}

func TestElevateStepWhenNeeded(t *testing.T) {
	r := newRig(t)
	r.admin = false
	r.mutate = func(a *fakeAction) { a.plan.NeedsAdmin = true }
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "image")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\card.img`)
	r.waitStep(t, StepOptions)
	r.do(t, "next")
	v := r.waitStep(t, StepElevate)
	if !strings.Contains(v.Instruction, "administrator") {
		t.Fatalf("instruction %q", v.Instruction)
	}
}

func TestSelectPathFolderOrImage(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	os.WriteFile(img, []byte("x"), 0o644)
	r.do(t, "select-path", "path", dir)
	v := r.s.View()
	if v.Step != StepAction || v.Card == nil || v.Card.Root != dir || v.Card.FS != "folder" {
		t.Fatalf("folder: %+v", v.Card)
	}
	off := map[string]string{}
	for _, a := range v.Actions {
		if !a.Enabled {
			off[a.Name] = a.Reason
		}
	}
	if off["image"] == "" || off["fix"] == "" || off["recover"] == "" || off["rescue"] != "" {
		t.Fatalf("folder actions off: %v", off)
	}
	r.do(t, "select-path", "path", img)
	off = map[string]string{}
	for _, a := range r.s.View().Actions {
		if !a.Enabled {
			off[a.Name] = a.Reason
		}
	}
	if off["image"] == "" || off["fix"] == "" || off["recover"] != "" {
		t.Fatalf("image file actions off: %v", off)
	}
	if err := r.s.Do(Command{Name: "select-path", Args: map[string]string{"path": filepath.Join(dir, "missing")}}); err == nil {
		t.Fatal("a missing path was accepted")
	}
}

func TestEditDestFromReview(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "edit-dest")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\elsewhere`)
	r.waitStep(t, StepOptions)
	r.do(t, "next")
	r.waitStep(t, StepReview)
	if req := r.last().req; req.Dest != `E:\elsewhere` || req.Backup != "full" {
		t.Fatalf("request %+v", req)
	}
}

// TestProgressCountersCopied: the engine keeps writing its counters map; the session must hold
// its own copy, or renderers reading it race with the engine (found while running the GUI).
func TestProgressCountersCopied(t *testing.T) {
	r := newRig(t)
	counters := map[string]int{"OK": 1}
	done := make(chan struct{})
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			env.Progress(job.Progress{Counters: counters})
			counters["OK"] = 99 // the engine carries on counting
			close(done)
			return job.Result{Clean: true}, nil
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	<-done
	r.waitStep(t, StepResults)
	if v := r.s.View(); v.Progress.Counters["OK"] != 1 {
		t.Fatalf("view shares the engine's map: %v", v.Progress.Counters)
	}
}

// Final review I7: a card tool this OS can't run on this card is greyed with the reason (not only
// refused on the check page), and can't be chosen.
func TestFixToolsGreyedWithReason(t *testing.T) {
	r := newRig(t)
	r.tools = []job.Capability{{Name: "Check", Enabled: true}, {Name: "Repair", Enabled: true},
		{Name: "Format", Reason: "mkfs.exfat not installed (exfatprogs)"}, {Name: "Health test", Enabled: true}}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	v := r.waitStep(t, StepOptions)
	if len(v.Tools) != 4 || v.Tools[2].Name != "format" || v.Tools[2].Enabled || !strings.Contains(v.Tools[2].Reason, "exfatprogs") || !v.Tools[0].Enabled {
		t.Fatalf("tools %+v", v.Tools)
	}
	if err := r.s.Do(Command{Name: "set-option", Args: map[string]string{"name": "fix-op", "value": "format"}}); err == nil || !strings.Contains(err.Error(), "exfatprogs") {
		t.Fatalf("chose an unavailable tool: %v", err)
	}
	r.do(t, "set-option", "name", "fix-op", "value", "check")
}

// Final review I7: on a card whose deleted records can't be read (NTFS), Undelete is shown greyed
// with the reason and Deep scan is on and stays on — it's the only way to find files there.
func TestRecoverUndeleteGreyedOnNTFS(t *testing.T) {
	nt := card
	nt.FS = "NTFS"
	r := newRig(t, nt)
	r.do(t, "select-drive", "root", nt.Root)
	r.do(t, "choose-action", "action", "recover")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\out`)
	v := r.waitStep(t, StepOptions)
	if v.Undelete == nil || v.Undelete.Enabled || !strings.Contains(v.Undelete.Reason, "NTFS") || !v.Req.Deep {
		t.Fatalf("undelete %+v deep %v", v.Undelete, v.Req.Deep)
	}
	if err := r.s.Do(Command{Name: "set-option", Args: map[string]string{"name": "deep", "value": "0"}}); err == nil {
		t.Fatal("deep scan turned off where it is the only way")
	}
	ex := newRig(t) // exFAT: undelete works, nothing greyed, deep is the person's choice
	ex.do(t, "select-drive", "root", card.Root)
	ex.do(t, "choose-action", "action", "recover")
	ex.waitStep(t, StepDest)
	ex.do(t, "set-dest", "path", `E:\out`)
	if v := ex.waitStep(t, StepOptions); v.Undelete != nil || v.Req.Deep {
		t.Fatalf("exFAT: undelete %+v deep %v", v.Undelete, v.Req.Deep)
	}
}
