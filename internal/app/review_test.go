package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
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
