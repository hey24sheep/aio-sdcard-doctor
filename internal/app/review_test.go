package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
)

func toRecoverList(t *testing.T, r *rig) {
	t.Helper()
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "recover")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\out`)
	r.waitStep(t, StepOptions)
	r.do(t, "next")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.waitStep(t, StepRecoverList)
}

// M4 review #1: Back from the results list must never land on a finished "run" step.
func TestBackFromRecoverListNeverReachesRun(t *testing.T) {
	r := newRig(t)
	toRecoverList(t, r)
	r.do(t, "back")
	v := r.waitStep(t, StepReview) // the list's plan again, ready to start
	if v.Plan == nil {
		t.Fatal("review without a plan after Back")
	}
	for i := 0; i < 6; i++ {
		if v := r.s.View(); v.Step == StepRun {
			t.Fatal("back reached the run step")
		}
		r.do(t, "back")
		time.Sleep(10 * time.Millisecond)
	}
	r.do(t, "restart")
	if v := r.s.View(); v.Step != StepDrive {
		t.Fatalf("restart: %s", v.Step)
	}
}

// M4 review #2: a slow pre-flight can be left (Back) or stopped (cancel), and Close doesn't wait for it.
func TestSlowPlanCanBeLeft(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) { a.slow = true }
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	if v := r.s.View(); v.Busy == "" {
		t.Fatal("not busy")
	}
	r.do(t, "back") // leaves the slow check at once: back on "What to copy", whose choices load quickly
	r.waitStep(t, StepChoice)
	r.do(t, "back")
	r.do(t, "quick-rescue")
	r.do(t, "cancel")
	if v := r.s.View(); v.Busy != "" {
		t.Fatalf("cancel left busy %q", v.Busy)
	}
	r.do(t, "quick-rescue")
	start := time.Now()
	r.s.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close waited %v for a plan", d)
	}
}

// M4 review #10: when a run can resume, the front ends can choose to start over instead.
func TestStartOverOption(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.plan.Issues = []job.Issue{{Kind: "resume", Title: "1 of 3 files already copied", Choices: []job.Choice{job.Resume, job.New}, Default: job.Resume}}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	v := r.waitStep(t, StepReview)
	if !v.CanStartOver {
		t.Fatal("start over not offered")
	}
	r.do(t, "set-option", "name", "fresh", "value", "1")
	r.waitStep(t, StepReview)
	if req := r.last().req; !req.Fresh {
		t.Fatalf("request %+v", req)
	}
}

// M4 review #13: Back from the quick path's check page shows the backup choices, loaded.
func TestQuickPathBackLoadsChoices(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "back")
	if v := r.waitStep(t, StepChoice); len(v.Choices) == 0 {
		t.Fatal("no choices after Back")
	}
}

// M4 review #15: the drive list is read without holding the session (a slow card can't freeze it).
func TestRefreshDoesNotBlockView(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s := NewSession(ctx, Deps{
		ListDrives: func() ([]drives.Drive, error) {
			calls++
			if calls > 1 {
				<-release
			}
			return []drives.Drive{card}, nil
		},
		NewAction: func(Request) (job.Action, error) { return &fakeAction{}, nil },
	})
	defer s.Close()
	defer once.Do(func() { close(release) })
	go s.Do(Command{Name: "refresh"})
	time.Sleep(50 * time.Millisecond)
	got := make(chan Step, 1)
	go func() { got <- s.View().Step }()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("View blocked while the drive list was being read")
	}
	once.Do(func() { close(release) })
}

// M4 review #17: typing a drive letter picks that drive, not a "folder".
func TestSelectPathDriveLetter(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-path", "path", "q:")
	if v := r.s.View(); v.Card == nil || v.Card.Root != card.Root || !v.Card.Removable {
		t.Fatalf("card %+v", v.Card)
	}
}

// Found live (M5): Back on the "needs administrator rights" page went to the check page, which
// planned again and landed on the admin page again — the person could never go back.
func TestBackFromElevateLeavesIt(t *testing.T) {
	r := newRig(t)
	r.admin = false
	r.mutate = func(a *fakeAction) { a.plan.NeedsAdmin = true }
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "image")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", `E:\card.img`)
	r.waitStep(t, StepOptions)
	r.do(t, "next")
	r.waitStep(t, StepElevate)
	r.do(t, "back")
	time.Sleep(100 * time.Millisecond) // a re-plan would have landed on elevate again by now
	r.waitStep(t, StepOptions)
}

// Adversarial pass U1: the typed confirmation was decided from the editable request. Changing
// the card tool on the check page (two requests out of order, or any client) let a format plan
// start with no typed label and size.
func TestTypedConfirmFollowsWhatWasPlanned(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "fix-op", "value", "format")
	r.do(t, "next")
	r.waitStep(t, StepReview)
	r.s.Do(Command{Name: "set-option", Args: map[string]string{"name": "fix-op", "value": "check"}}) // must not change the plan
	if err := r.s.Do(Command{Name: "start"}); err == nil {
		t.Fatal("the planned format started without the typed label and size")
	}
}

// Adversarial pass (minor): a double-clicked Start left "finish or stop the current run first"
// on the run page until the run ended.
func TestSecondStartLeavesNoError(t *testing.T) {
	r := newRig(t)
	block := make(chan struct{})
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return job.Result{Clean: true}, nil
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.s.Do(Command{Name: "start"}) // the second click
	if v := r.s.View(); v.Error != "" {
		t.Fatalf("error on the run page: %q", v.Error)
	}
	close(block)
}

// Adversarial pass (minor): Back from the scan's check page jumped to the card list, and Back
// after a "What next" choice went to the old run's results. Both go to "What to do".
func TestBackGoesToWhatToDo(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "scan")
	r.waitStep(t, StepReview)
	r.do(t, "back")
	if st := r.s.View().Step; st != StepAction {
		t.Fatalf("Back from the scan check went to %s", st)
	}
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.waitStep(t, StepResults)
	r.do(t, "next-action", "action", "verify")
	r.do(t, "back")
	if st := r.s.View().Step; st != StepAction {
		t.Fatalf("Back after What next went to %s", st)
	}
}

// Adversarial pass 2 OU1: after a full backup image, "Copy files from the image" read the card
// (the request still named the card). It copies from the image file, which the card can rest.
func TestCopyFromImageReadsTheImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "card.img")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "image")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", img)
	r.waitStep(t, StepOptions)
	r.do(t, "next")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepResults)
	if len(v.Next) != 1 || v.Next[0].Action != "rescue" {
		t.Fatalf("next %+v", v.Next)
	}
	r.do(t, "next-action", "action", "rescue")
	v = r.waitStep(t, StepChoice)
	if v.Req.From != img || v.Card == nil || v.Card.Root != img || v.Card.FS != KindImage {
		t.Fatalf("copy from the image reads %q (card %+v)", v.Req.From, v.Card)
	}
}

// Adversarial pass 2 OU1: a compressed image keeps no raw card.img (it is removed once the .xz is
// checked), and a stopped image is incomplete: neither offers copying from the image.
func TestNoCopyFromImageWithoutTheRawImage(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "image")
	r.waitStep(t, StepDest)
	r.do(t, "set-dest", "path", filepath.Join(t.TempDir(), "card.img")) // never written: only the .xz is left
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "compress", "value", "1")
	r.do(t, "next")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	if v := r.waitStep(t, StepResults); slices.ContainsFunc(v.Next, func(n NextStep) bool { return n.Action == "rescue" }) {
		t.Fatalf("offered to copy from an image that isn't there: %+v", v.Next)
	}
	if err := r.s.Do(Command{Name: "next-action", Args: map[string]string{"action": "rescue"}}); err == nil {
		t.Fatal("started copying from an image that isn't there")
	}
}

// Adversarial pass 2 OU3: after picking files, going back before the list (to the check, the
// options or the folder) must look again, not extract the old picks with the new settings.
func TestRecoverBackBeforeListListsAgain(t *testing.T) {
	r := newRig(t)
	toRecoverList(t, r)
	r.do(t, "recover-tick-all")
	r.do(t, "recover-start")
	r.waitStep(t, StepReview)
	r.do(t, "back") // the pick list: the picks stay
	if v := r.s.View(); v.Step != StepRecoverList || v.Recover == nil || v.Recover.Ticked != 1 {
		t.Fatalf("back to the list: %s %+v", v.Step, v.Recover)
	}
	r.do(t, "back") // the listing's check
	r.waitStep(t, StepReview)
	if req := r.last().req; !req.List || req.Select != nil {
		t.Fatalf("the check before the list plans %+v", req)
	}
	r.do(t, "back")
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "deep", "value", "1")
	r.do(t, "next")
	r.waitStep(t, StepReview)
	if req := r.last().req; !req.List || req.Select != nil || !req.Deep {
		t.Fatalf("options changed after Back, but the check is %+v", req)
	}

	// "Change where the files go" on the extraction check also starts a new listing
	r2 := newRig(t)
	toRecoverList(t, r2)
	r2.do(t, "recover-tick-all")
	r2.do(t, "recover-start")
	r2.waitStep(t, StepReview)
	r2.do(t, "edit-dest")
	r2.do(t, "set-dest", "path", `E:\elsewhere`)
	r2.do(t, "next")
	r2.waitStep(t, StepReview)
	if req := r2.last().req; !req.List || req.Select != nil {
		t.Fatalf("new folder, old picks: %+v", req)
	}
}

// Adversarial pass 2 OU4: "Make an image instead" stopped the copy and then offered "Check the
// copy" under "Stopped". It says what happened and offers the image of the same card.
func TestImageInsteadOffersTheImage(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(context.Context, job.Env) (job.Result, error) { return job.Result{}, rescue.ErrImageInstead }
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	v := r.waitStep(t, StepResults)
	if !v.ImageInstead || v.Error != "" || len(v.Next) != 1 || v.Next[0].Action != "image" {
		t.Fatalf("image instead: flag %v error %q next %+v", v.ImageInstead, v.Error, v.Next)
	}
	r.do(t, "next-action", "action", "image")
	v = r.waitStep(t, StepDest)
	if v.Req.Action != "image" || v.Req.From != card.Root || v.ImageInstead {
		t.Fatalf("image request %+v (flag %v)", v.Req, v.ImageInstead)
	}
}

// Adversarial pass 2 OU6: Start over after a failed run showed the old run's problem on the
// card list.
func TestRestartClearsTheOldProblem(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(context.Context, job.Env) (job.Result, error) {
			return job.Result{}, errors.New("the card stopped responding")
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	if v := r.waitStep(t, StepResults); v.Error == "" {
		t.Fatal("no error to begin with")
	}
	r.do(t, "restart")
	if v := r.s.View(); v.Step != StepDrive || v.Error != "" {
		t.Fatalf("after Start over: %s %q", v.Step, v.Error)
	}
}

// Adversarial pass 3 SU1: a pasted destination with its quotes is used without them; a path no
// folder can have is refused with the reason instead of reaching the copy's write guard.
func TestPastedDestinationCheckedBeforePlan(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "rescue")
	r.waitStep(t, StepChoice)
	r.do(t, "set-choice", "id", "full")
	r.do(t, "set-dest", "path", `"`+dir+`"`)
	if v := r.s.View(); v.Step != StepOptions || v.Req.Dest != dir {
		t.Fatalf("quoted path: step %s dest %q", v.Step, v.Req.Dest)
	}
	r.do(t, "back")
	bad := []string{dir + `\"x"`}
	if runtime.GOOS == "windows" {
		bad = append(bad, dir+`\aux`, dir+`\a:b`)
	}
	for _, p := range bad {
		err := r.s.Do(Command{Name: "set-dest", Args: map[string]string{"path": p}})
		if err == nil || r.s.View().Step != StepDest {
			t.Fatalf("%q accepted (err %v, step %s)", p, err, r.s.View().Step)
		}
	}
}

// Adversarial pass 3 SU1: the image file's folder gets the same checks, and an image file on
// the card being imaged is refused like any other destination there.
func TestImageDestinationChecked(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "image")
	r.waitStep(t, StepDest)
	bad := []string{`"` + filepath.Join(t.TempDir(), `x"y`, "card.img") + `"`}
	if runtime.GOOS == "windows" { // the fake card's root is a drive letter
		bad = append(bad, card.Root+"card.img")
	}
	for _, p := range bad {
		if err := r.s.Do(Command{Name: "set-dest", Args: map[string]string{"path": p}}); err == nil {
			t.Fatalf("image file %q accepted", p)
		}
	}
	img := filepath.Join(t.TempDir(), "card.img")
	r.do(t, "set-dest", "path", `"`+img+`"`)
	if v := r.s.View(); v.Req.Dest != img {
		t.Fatalf("image dest %q", v.Req.Dest)
	}
}

// Adversarial pass 3 SU1: a panic in the background (planning, running, looking at the card)
// ended the whole program — the GUI died with a stale lock. It becomes a problem on the page.
func TestPanicInBackgroundIsAProblemNotACrash(t *testing.T) {
	r := newRig(t)
	r.mutate = func(a *fakeAction) {
		a.run = func(context.Context, job.Env) (job.Result, error) { panic("boom in run") }
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	if v := r.waitStep(t, StepResults); !strings.Contains(v.Error, "Something went wrong") || !strings.Contains(v.Error, "boom in run") {
		t.Fatalf("run panic: error %q", v.Error)
	}
	r.do(t, "restart")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSession(ctx, Deps{
		ListDrives: func() ([]drives.Drive, error) { return []drives.Drive{card}, nil },
		NewAction:  func(Request) (job.Action, error) { panic("boom in plan") },
		Choices:    func(context.Context, string) ([]ChoiceRow, error) { panic("boom in choices") },
		FixTools:   func(string) []job.Capability { panic("boom in tools") },
		IsAdmin:    func() bool { return true },
	})
	defer s.Close()
	waitErr := func(want string) {
		t.Helper()
		for i := 0; i < 1000; i++ {
			if v := s.View(); v.Busy == "" && strings.Contains(v.Error, want) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no %q problem: %+v", want, s.View().Error)
	}
	do := func(name string, kv ...string) {
		args := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			args[kv[i]] = kv[i+1]
		}
		s.Do(Command{Name: name, Args: args})
	}
	do("select-drive", "root", card.Root)
	do("choose-action", "action", "rescue")
	waitErr("boom in choices")
	do("back")
	do("choose-action", "action", "fix")
	waitErr("boom in tools")
	do("back")
	do("choose-action", "action", "scan")
	waitErr("boom in plan")
}

// Drives that don't answer when listed (fixer C, round 3) are listed as "not responding": only
// reading the card itself is offered there, with the reason in words.
func TestNotRespondingDriveOffersOnlyReadingTheCard(t *testing.T) {
	d := drives.Drive{Root: `R:\`, Device: `\\.\R:`, FS: KindNotResponding, RAW: true}
	for _, a := range actionsFor(d) {
		want := a.Name == "image" || a.Name == "recover" || a.Name == "verify"
		if a.Enabled != want || !a.Enabled && !strings.Contains(a.Reason, "didn't answer") {
			t.Errorf("%s: enabled %v (%q)", a.Name, a.Enabled, a.Reason)
		}
	}
}

// Adversarial pass 3 SU2/OU3: card B's "What to copy" showed card A's counts (while scanning,
// after a cancel, or after the quick path and Back).
func TestChoicesBelongToTheCardShown(t *testing.T) {
	cardB := drives.Drive{Root: `R:\`, Label: "OTHER", FS: "exFAT", Total: 32 << 30, Removable: true}
	r := newRig(t, card, cardB)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "rescue")
	if v := r.waitStep(t, StepChoice); len(v.Choices) == 0 {
		t.Fatal("no choices for the first card")
	}
	r.do(t, "back")
	r.do(t, "back")
	r.do(t, "select-drive", "root", cardB.Root)
	if v := r.s.View(); len(v.Choices) != 0 {
		t.Fatalf("card B shows card A's choices: %+v", v.Choices)
	}
	r.do(t, "back")
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "rescue")
	r.waitStep(t, StepChoice)
	r.do(t, "back")
	r.do(t, "choose-action", "action", "scan") // a new action starts with no counts of the old one
	if v := r.s.View(); len(v.Choices) != 0 {
		t.Fatalf("scan carries the rescue's counts: %+v", v.Choices)
	}
}

// Adversarial pass 3 SU3: a refused command on the results page turned "Done" into "Stopped"
// with a Problem banner; and a banner from a refused Continue stayed after the person fixed it.
func TestRefusedCommandNeverRewritesTheResult(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.waitStep(t, StepResults)
	if err := r.s.Do(Command{Name: "start"}); err == nil { // a second window's late Start
		t.Fatal("start accepted on the results page")
	}
	if v := r.s.View(); v.Error != "" {
		t.Fatalf("results now say %q", v.Error)
	}
	r.do(t, "restart")
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	r.waitStep(t, StepOptions)
	if err := r.s.Do(Command{Name: "next"}); err == nil || r.s.View().Error == "" {
		t.Fatalf("Continue with no tool chosen: %v %q", err, r.s.View().Error)
	}
	r.do(t, "set-option", "name", "fix-op", "value", "check")
	if v := r.s.View(); v.Error != "" {
		t.Fatalf("the old problem stays after it was fixed: %q", v.Error)
	}
}

// Adversarial pass 3 SU4: a second click on Start while it runs is nothing to report (the GUI
// showed "Not done: finish or stop the current run first" for the whole run).
func TestSecondStartIsNotAnError(t *testing.T) {
	r := newRig(t)
	block := make(chan struct{})
	r.mutate = func(a *fakeAction) {
		a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return job.Result{Clean: true}, nil
		}
	}
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "quick-rescue")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	err := r.s.Do(Command{Name: "start"})
	close(block)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
}

// Adversarial pass 3 OU9: a failed drive listing left "Problem:" on the card list after the
// next listing worked.
func TestDriveListProblemClearedByAGoodListing(t *testing.T) {
	var fail atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSession(ctx, Deps{
		ListDrives: func() ([]drives.Drive, error) {
			if fail.Load() {
				return nil, errors.New("device not ready")
			}
			return []drives.Drive{card}, nil
		},
		NewAction: func(req Request) (job.Action, error) { return &fakeAction{req: req}, nil },
	})
	defer s.Close()
	fail.Store(true)
	s.Do(Command{Name: "refresh"})
	if v := s.View(); !strings.Contains(v.Error, "device not ready") {
		t.Fatalf("failed listing not shown: %q", v.Error)
	}
	fail.Store(false)
	s.Do(Command{Name: "refresh"})
	if v := s.View(); v.Error != "" || len(v.Drives) != 1 {
		t.Fatalf("after a good listing: %q (%d drives)", v.Error, len(v.Drives))
	}
}

// Adversarial pass 3 SU8/OU10: Start over listed the drives, and a typed path was looked up,
// while holding the session: a slow drive froze every window's view.
func TestNoDiskLookupWhileHoldingTheSession(t *testing.T) {
	var slow atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSession(ctx, Deps{
		ListDrives: func() ([]drives.Drive, error) {
			if slow.Load() {
				time.Sleep(800 * time.Millisecond)
			}
			return []drives.Drive{card}, nil
		},
		NewAction: func(req Request) (job.Action, error) { return &fakeAction{req: req}, nil },
	})
	defer s.Close()
	s.Do(Command{Name: "select-drive", Args: map[string]string{"root": card.Root}})
	slow.Store(true)
	done := make(chan struct{})
	go func() { s.Do(Command{Name: "restart"}); close(done) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	s.View()
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("View waited %v for Start over's drive listing", d)
	}
	<-done
	if v := s.View(); v.Step != StepDrive || len(v.Drives) != 1 {
		t.Fatalf("after Start over: %s, %d drives", v.Step, len(v.Drives))
	}

	statted := make(chan struct{})
	old := statFn
	defer func() { statFn = old }()
	statFn = func(p string) (os.FileInfo, error) {
		close(statted)
		time.Sleep(800 * time.Millisecond)
		return os.Stat(p)
	}
	dir := t.TempDir()
	go s.Do(Command{Name: "select-path", Args: map[string]string{"path": dir}})
	<-statted
	start = time.Now()
	s.View()
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("View waited %v for a typed path's lookup", d)
	}
	for i := 0; i < 400 && s.View().Step != StepAction; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if v := s.View(); v.Card == nil || v.Card.Root != dir || v.Card.FS != KindFolder {
		t.Fatalf("typed folder: %+v", v.Card)
	}
}

// Adversarial pass 3 OU13: stopping "Checking which card tools work here" left every tool
// enabled, as if all had been checked.
func TestStoppedToolCheckLeavesToolsUnchecked(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.s.deps.FixTools = func(string) []job.Capability { <-release; return nil }
	defer close(release)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	r.do(t, "cancel")
	v := r.s.View()
	for _, tl := range v.Tools {
		if tl.Enabled {
			t.Fatalf("%s enabled though never checked", tl.Name)
		}
	}
	if err := r.s.Do(Command{Name: "set-option", Args: map[string]string{"name": "fix-op", "value": "format"}}); err == nil {
		t.Fatal("an unchecked tool could be chosen")
	}
}

// Adversarial pass 3 OU11: a rescue checked in this window (verify: all files match) counts for
// the card tools, as the README says — the window passes it on like the command line's --manifest.
func TestVerifiedRescueReachesTheCardTool(t *testing.T) {
	r := newRig(t)
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "verify")
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "manifest", "value", `D:\rescue\Reports\manifest.json`)
	r.do(t, "next")
	r.waitStep(t, StepReview)
	r.do(t, "start")
	r.waitStep(t, StepResults)
	r.do(t, "restart")
	r.do(t, "select-drive", "root", card.Root)
	r.do(t, "choose-action", "action", "fix")
	r.waitStep(t, StepOptions)
	r.do(t, "set-option", "name", "fix-op", "value", "check")
	r.do(t, "next")
	r.waitStep(t, StepReview)
	if m := r.last().req.Manifest; m != `D:\rescue\Reports\manifest.json` {
		t.Fatalf("card tool planned with manifest %q", m)
	}
}

// Adversarial pass 3 OU2: Stop during a repair or format lets the system tool finish; when it
// then reports its own result, that result is shown, not "Stopped… run it again to continue".
func TestStoppedCardToolShowsTheToolsResult(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{nil, ""}, {errors.New("chkdsk could not fix the errors"), "chkdsk could not fix"}} {
		r := newRig(t)
		started := make(chan struct{})
		r.mutate = func(a *fakeAction) {
			a.run = func(ctx context.Context, env job.Env) (job.Result, error) {
				close(started)
				<-ctx.Done() // Stop pressed; the tool goes on to its end
				return job.Result{Clean: tc.err == nil}, tc.err
			}
		}
		r.do(t, "select-drive", "root", card.Root)
		r.do(t, "choose-action", "action", "fix")
		r.waitStep(t, StepOptions)
		r.do(t, "set-option", "name", "fix-op", "value", "check")
		r.do(t, "next")
		r.waitStep(t, StepReview)
		r.do(t, "start")
		<-started
		r.do(t, "cancel")
		v := r.waitStep(t, StepResults)
		if tc.want == "" && v.Error != "" || tc.want != "" && !strings.Contains(v.Error, tc.want) {
			t.Fatalf("tool said %v; the page says %q", tc.err, v.Error)
		}
	}
}
