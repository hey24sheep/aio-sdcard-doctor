package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
)

// I4: --raw with a drive letter reads that volume's device, not the folder.
func TestRawDriveLetterMapsToVolumeDevice(t *testing.T) {
	old := lookupDrive
	defer func() { lookupDrive = old }()
	lookupDrive = func(p string) (drives.Drive, error) {
		if p == `E:\` {
			return drives.Drive{Root: `E:\`, Device: `\\.\E:`}, nil
		}
		return drives.Drive{}, errors.New("not found")
	}
	if got := rawSource(`E:\`); got != `\\.\E:` {
		t.Fatalf("got %q", got)
	}
	if got := rawSource(`\\.\PhysicalDrive2`); got != `\\.\PhysicalDrive2` {
		t.Fatalf("device path changed: %q", got)
	}
}

// I11: without a terminal, waiting for a removed card is bounded.
func TestImageWaitBoundedWithoutTTY(t *testing.T) {
	if d := waitTimeout(IO{TTY: false}); d != 10*time.Minute {
		t.Fatalf("non-TTY wait %v", d)
	}
	if d := waitTimeout(IO{TTY: true}); d != 0 {
		t.Fatalf("TTY wait %v", d)
	}
}

// Adversarial pass 2 OU5: card-controlled text (file names, labels, report paths) reached the
// terminal raw — an escape sequence in a name could rewrite the screen or set the clipboard.
func TestCLICleansCardText(t *testing.T) {
	evil := "DCIM\x1b]52;c;cm0gLXJmIH4K\x07/a\u202ejpg.exe\r"
	var out, errOut bytes.Buffer
	io := IO{In: strings.NewReader("\n"), Out: &out, Err: &errOut, TTY: true}
	p := &printer{io: io, plain: true}
	p.log("warn", "[FAILED] "+evil+" read error")
	p.progress(job.Progress{Phase: "Copying", Item: evil, Total: 1})
	(&ttyPrompter{io: io}).Ask(context.Background(), job.Issue{Title: "Can't read " + evil, Detail: evil,
		Choices: []job.Choice{job.Retry, job.Stop}, Default: job.Retry})
	printResult(io, job.Result{Counters: map[string]int{"OK": 1}, Reports: []string{evil}})
	confirm(context.Background(), IO{Out: &out, Err: &errOut}, &globals{}, job.Plan{Summary: []string{"From: " + evil}, Result: []string{evil}})
	out.WriteString(driveLine(drives.Drive{Root: `Q:\`, Label: "EVIL\x1b[2J"}))
	for _, s := range []string{out.String(), errOut.String()} {
		if strings.ContainsAny(s, "\x1b\x07\r\u202e") {
			t.Errorf("card text written raw: %q", s)
		}
	}
}

// Adversarial pass 2 OU8: Ctrl+C at a menu or question exited 2 (or 4 at the administrator
// question) instead of the 130 the README promises for an interrupted run.
func TestInterruptedExitIs130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{exit(ExitUsage, "%v", errNoAnswer), exit(ExitAdmin, "Not elevated.\n  run it again"), exit(ExitError, "Cancelled — nothing was started.")} {
		var e bytes.Buffer
		if code := exitCode(ctx, err, IO{Err: &e}); code != ExitInterrupted || !strings.Contains(e.String(), "Interrupted") {
			t.Errorf("%v after Ctrl+C: exit %d %q", err, code, e.String())
		}
	}
	var e bytes.Buffer
	if code := exitCode(context.Background(), exit(ExitAdmin, "Not elevated."), IO{Err: &e}); code != ExitAdmin {
		t.Errorf("without Ctrl+C: exit %d", code)
	}
}

// Adversarial pass 2 OU4: "Make a full backup image instead" ended the copy with exit 1 and no
// word on how to make that image.
func TestImageInsteadSaysHowToMakeTheImage(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip(`drive letters: elsewhere "E:\" is a relative folder on the runner's own disk`)
	}
	r := &rescue.Rescue{From: `E:\`, Dest: `D:\rescue`}
	got := imageInsteadHint(r)
	want := `aio-sdcard-doctor image --from "E:" --to "` + filepath.Join(`D:\rescue`, "card.img") + `"`
	if !strings.Contains(got, want) {
		t.Fatalf("hint %q lacks %q", got, want)
	}
}
