package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
)

// Adversarial pass 3 SU7/OD10: --plain lost to the double-click check, so `--plain` from a
// script with a hidden console got a GUI server and no output.
func TestPlainWinsOverDoubleClick(t *testing.T) {
	if got := launchMode(true, false, true); got != "usage" {
		t.Fatalf("--plain when started without a terminal: %s", got)
	}
	if got := launchMode(true, true, true); got != "usage" {
		t.Fatalf("--plain in a fresh console: %s", got)
	}
}

// Adversarial pass 3 SU7/OD10: output sent to a pipe or a file is a script, never a double-click.
func TestRedirectedOutputIsNeverADoubleClick(t *testing.T) {
	for _, c := range []struct {
		procs int
		out   outKind
		want  bool
	}{
		{1, outConsole, true}, // Explorer: a fresh console of its own
		{0, outNone, true},    // the windowed build: no console, no output at all
		{2, outConsole, false},
		{1, outPipe, false}, // a script reading the output, its console hidden
		{0, outPipe, false},
		{1, outFile, false}, // > out.txt
		{1, outOther, false},
	} {
		if got := startedByDoubleClick(c.procs, c.out); got != c.want {
			t.Errorf("%d console processes, output %v: %v, want %v", c.procs, c.out, got, c.want)
		}
	}
}

type stoppedAction struct {
	name string
	res  job.Result
	err  error
}

func (a stoppedAction) Name() string { return a.name }
func (a stoppedAction) Plan(context.Context, job.Env) (job.Plan, error) {
	return job.Plan{Summary: []string{"From: Q:"}}, nil
}
func (a stoppedAction) Run(context.Context, job.Env, job.Plan) (job.Result, error) {
	return a.res, a.err
}

// Adversarial pass 3 OU6: a run that ended with an error printed neither what it copied nor
// where its report is.
func TestErrorRunStillPrintsItsResult(t *testing.T) {
	var out, errb bytes.Buffer
	io := IO{In: strings.NewReader(""), Out: &out, Err: &errb}
	g := &globals{yes: true}
	a := stoppedAction{name: "rescue", res: job.Result{Counters: map[string]int{"OK": 900, "FAILED": 3}, Reports: []string{`D:\rescue\Reports\copy_1.csv`}},
		err: errors.New("stopped: the card stopped responding")}
	err := runPlanned(context.Background(), io, g, a, g.env(io), job.Plan{Summary: []string{"From: Q:"}}, nil, nil)
	exitCode(context.Background(), err, io)
	if o := out.String(); !strings.Contains(o, "copy_1.csv") || !strings.Contains(o, "900") || !strings.Contains(errb.String(), "stopped responding") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", o, errb.String())
	}
}

// Adversarial pass 3 OU2: Ctrl+C during a repair or format lets the system tool finish; what it
// then reports is the outcome — not "Interrupted… run it again to resume" and exit 130.
func TestStoppedCardToolReportsTheToolsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Ctrl+C while the tool ran
	for _, c := range []struct {
		a    stoppedAction
		code int
		say  string
	}{
		{stoppedAction{name: "fix", res: job.Result{Counters: map[string]int{"repaired": 1}, Clean: true}}, ExitOK, "repaired"},
		{stoppedAction{name: "fix", err: errors.New("chkdsk could not fix the errors")}, ExitError, "could not fix"},
		{stoppedAction{name: "fix", err: context.Canceled}, ExitInterrupted, "Interrupted"}, // the health test stops at a block
		{stoppedAction{name: "rescue", err: context.Canceled}, ExitInterrupted, "Interrupted"},
	} {
		var out, errb bytes.Buffer
		io := IO{In: strings.NewReader(""), Out: &out, Err: &errb}
		g := &globals{yes: true}
		err := runPlanned(ctx, io, g, c.a, g.env(io), job.Plan{}, nil, nil)
		if code := exitCode(ctx, err, io); code != c.code || !strings.Contains(out.String()+errb.String(), c.say) {
			t.Errorf("%s returned %v: exit %d\n%s%s", c.a.name, c.a.err, code, out.String(), errb.String())
		}
	}
}

// Adversarial pass 3 OU7: the "image instead" hint after `rescue --raw --from E:` (or from a
// folder on the card) named the partition or the folder: a full backup image is of the disk.
func TestImageInsteadHintNamesTheWholeDisk(t *testing.T) {
	old := drives.Lister
	defer func() { drives.Lister = old }()
	drives.Lister = func() ([]drives.Drive, error) { // a made-up card: nothing real is looked at
		return []drives.Drive{{Root: `Q:\`, Device: `\\.\Q:`, PhysicalID: "PhysicalDrive7", Removable: true}}, nil
	}
	disk := `\\.\PhysicalDrive7`
	if runtime.GOOS != "windows" {
		disk = "/dev/PhysicalDrive7"
	}
	froms := []string{`\\.\Q:`}
	if runtime.GOOS == "windows" {
		froms = append(froms, `Q:\`, `Q:\DCIM`)
	}
	for _, from := range froms {
		h := imageInsteadHint(&rescue.Rescue{From: from, Dest: `D:\rescue`})
		if want := `image --from "` + disk + `" --to "` + filepath.Join(`D:\rescue`, "card.img") + `"`; !strings.Contains(h, want) {
			t.Errorf("from %s: hint %q, want %q", from, h, want)
		}
	}
}

// Adversarial pass 3 SU11: Ctrl+C at a question is its safe answer: Stop when offered, else the
// default (never a choice the question doesn't have).
func TestCtrlCAtAQuestionIsTheSafeAnswerCLI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &ttyPrompter{io: IO{In: strings.NewReader(""), Err: &bytes.Buffer{}}}
	if c := p.Ask(ctx, job.Issue{Choices: []job.Choice{job.Wait, job.Stop}, Default: job.Wait}); c != job.Stop {
		t.Errorf("with Stop offered: %s", c)
	}
	if c := p.Ask(ctx, job.Issue{Choices: []job.Choice{job.Resume, job.New}, Default: job.Resume}); c != job.Resume {
		t.Errorf("resume or start over: %s", c)
	}
}

// Adversarial pass 3 OD15: the last progress line (Done == Total) fell inside the 1 s throttle,
// so a finished verify ended on "0%"; and "1 KB" in the plan was "0 KB" in progress.
func TestFinalProgressAlwaysPrinted(t *testing.T) {
	var out bytes.Buffer
	p := &printer{io: IO{Out: &out}, plain: true}
	p.progress(job.Progress{Phase: "Checking", Done: 0, Total: 700})
	p.progress(job.Progress{Phase: "Checking", Done: 700, Total: 700})
	if o := out.String(); !strings.Contains(o, "100%") || !strings.Contains(o, "1 KB / 1 KB") {
		t.Fatalf("progress:\n%s", o)
	}
}

// Adversarial pass 3 OD14: the help said "The card is only ever read." right above the command
// that repairs and formats it.
func TestHelpSaysWhoChangesTheCard(t *testing.T) {
	_, out, _ := cli(t, false, "", "--help")
	if !strings.Contains(out, "except by fix (Card tools), which asks you to type the card's label and size first") {
		t.Fatalf("help:\n%s", out)
	}
}

// Adversarial pass 3 SU10: `gui --port N` while a GUI runs reopened that one without saying the
// port asked for wasn't used.
func TestGuiPortIgnoredWhenReopening(t *testing.T) {
	oldR, oldO := reopenGUI, openWindow
	defer func() { reopenGUI, openWindow = oldR, oldO }()
	reopenGUI = func(string, bool) (string, bool) { return "http://127.0.0.1:5555/?t=x", true }
	openWindow = func(string) error { return nil }
	var out, errb bytes.Buffer
	if err := startGUI(context.Background(), IO{Out: &out, Err: &errb}, true, 6000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String()+errb.String(), "--port 6000 was ignored") {
		t.Fatalf("output:\n%s%s", out.String(), errb.String())
	}
}
