package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/verify"
)

// M4 review #6: what the OS tool says (the problems a check found) reaches the person.
func TestFixToolOutputLogged(t *testing.T) {
	tg := fix.Target{Vol: drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "FAT32", Total: 16 << 30, Removable: true, ID: drives.Identity{FSSerial: "1234-ABCD"}},
		Disk: drives.Device{Path: `\.\MOCK7`, Size: 16 << 30, Removable: true}}
	oldT, oldE := fixTargetFn, fix.Exec
	defer func() { fixTargetFn, fix.Exec = oldT, oldE }()
	fixTargetFn = func(string) (fix.Target, error) { return tg, nil } // made-up card: nothing real is looked at
	fix.Exec = func(_ context.Context, argv []string, out io.Writer) error {
		io.WriteString(out, "Windows found problems with the file system.\nCorrupt directory entry: DCIM\n")
		return nil
	}
	var logs []string
	env := job.Env{Now: time.Now, Prompt: job.AutoPrompter{}, Log: func(_, m string) { logs = append(logs, m) }}
	a := &fixAction{Fix: &fix.Fix{Op: fix.OpCheck, Target: tg, GOOS: "windows", Admin: func() bool { return true }}, from: `Q:\`}
	if _, err := a.Run(context.Background(), env, job.Plan{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Corrupt directory entry: DCIM") {
		t.Fatalf("logs %q", logs)
	}
}

// M4 review #10: "start over" reaches the action.
func TestFreshWired(t *testing.T) {
	a, _ := NewAction(Request{Action: "rescue", From: t.TempDir(), Dest: t.TempDir(), Backup: "full", Fresh: true})
	if r := a.(*rescue.Rescue); !r.Fresh {
		t.Fatal("rescue not fresh")
	}
	v, _ := NewAction(Request{Action: "verify", Manifest: "m.json", Fresh: true})
	if !v.(*verify.Verify).Restart {
		t.Fatal("verify not restarted")
	}
}

// M4 review #9: the administrator instruction names the front end in use.
func TestInstructionNamesFrontEnd(t *testing.T) {
	if s := ProductionDeps("tui").Instruction(nil); !strings.Contains(s, "tui") || strings.Contains(s, " gui") {
		t.Fatalf("tui: %q", s)
	}
}

// Adversarial pass 2 SA-5: a card with no volume (after a health test) has no label or serial,
// so a blank card of the same size put in the same reader while the person typed the
// confirmation went unnoticed. What is on its first sectors, read when the check is made, is
// read again before anything runs.
func TestFixVolumelessCardSwapNoticed(t *testing.T) {
	tg := fix.Target{Disk: drives.Device{Path: `\.\PhysicalDriveMOCK7`, Size: 64 << 30, Removable: true}}
	oldT, oldF, oldL, oldE := fixTargetFn, diskFingerprintFn, drives.Lister, fix.Exec
	defer func() { fixTargetFn, diskFingerprintFn, drives.Lister, fix.Exec = oldT, oldF, oldL, oldE }()
	fixTargetFn = func(string) (fix.Target, error) { return tg, nil } // made-up card: nothing real is looked at
	drives.Lister = func() ([]drives.Drive, error) { return nil, nil }
	reached := false
	fix.Exec = func(context.Context, []string, io.Writer) error {
		reached = true
		return errors.New("test: no real tool")
	}
	inReader, read := "the tested card", []string{}
	diskFingerprintFn = func(dev string) string { read = append(read, dev); return inReader }
	a, err := NewAction(Request{Action: "fix", FixOp: fix.OpFormat, From: tg.Disk.Path})
	if err != nil {
		t.Fatal(err)
	}
	inReader = "another blank card"
	env := job.Env{Now: time.Now, Prompt: job.AutoPrompter{}, Log: func(string, string) {}}
	_, err = a.Run(context.Background(), env, job.Plan{})
	if err == nil || !strings.Contains(err.Error(), "changed") || reached {
		t.Fatalf("swapped card: err %v, tool reached %v", err, reached)
	}
	if len(read) != 2 || read[0] != tg.Disk.Path || read[1] != tg.Disk.Path {
		t.Fatalf("fingerprints read from %q", read)
	}
}

// Adversarial pass 3 OA2: a card whose volume has no file-system serial (RAW, or a reader that
// reports none) was taken as "the same card" by size alone. Its first sectors are read when the
// check is made and again right before the tool runs.
func TestFixSerialLessCardSwapNoticed(t *testing.T) {
	tg := fix.Target{Vol: drives.Drive{Root: `Q:\`, Device: `\\.\Q:`, FS: "RAW", Total: 64 << 30, Removable: true},
		Disk: drives.Device{Path: `\\.\PhysicalDriveMOCK7`, Size: 64 << 30, Removable: true}}
	oldT, oldF, oldL, oldE := fixTargetFn, diskFingerprintFn, drives.Lister, fix.Exec
	defer func() { fixTargetFn, diskFingerprintFn, drives.Lister, fix.Exec = oldT, oldF, oldL, oldE }()
	fixTargetFn = func(string) (fix.Target, error) { return tg, nil } // made-up card: nothing real is looked at
	drives.Lister = func() ([]drives.Drive, error) { return nil, nil }
	reached := false
	fix.Exec = func(context.Context, []string, io.Writer) error {
		reached = true
		return errors.New("test: no real tool")
	}
	inReader, read := "the card checked", []string{}
	diskFingerprintFn = func(dev string) string { read = append(read, dev); return inReader }
	a, err := NewAction(Request{Action: "fix", FixOp: fix.OpFormat, From: tg.Vol.Root})
	if err != nil {
		t.Fatal(err)
	}
	inReader = "another card of the same size"
	env := job.Env{Now: time.Now, Prompt: job.AutoPrompter{}, Log: func(string, string) {}}
	_, err = a.Run(context.Background(), env, job.Plan{})
	if err == nil || !strings.Contains(err.Error(), "changed") || reached {
		t.Fatalf("swapped card: err %v, tool reached %v (fingerprints read from %q)", err, reached, read)
	}
}

// Adversarial pass 3 OU11: the window hands the card tool the rescue verified in it; the tool
// counts it only if the shared rule (the command line's --manifest rule) says it is of this card.
func TestFixCountsTheWindowsVerifiedRescue(t *testing.T) {
	tg := fix.Target{Vol: drives.Drive{Root: `Q:\`, FS: "exFAT", Total: 64 << 30, Removable: true, ID: drives.Identity{FSSerial: "1234-ABCD"}},
		Disk: drives.Device{Path: `\\.\PhysicalDriveMOCK7`, Size: 64 << 30, Removable: true}}
	oldT, oldV := fixTargetFn, verifiedRescueFn
	defer func() { fixTargetFn, verifiedRescueFn = oldT, oldV }()
	fixTargetFn = func(string) (fix.Target, error) { return tg, nil }
	var asked string
	verifiedRescueFn = func(m string, _ fix.Target) (bool, string) { asked = m; return m == `D:\r\Reports\manifest.json`, "" }
	for m, want := range map[string]bool{`D:\r\Reports\manifest.json`: true, `D:\other\Reports\manifest.json`: false} {
		a, err := NewAction(Request{Action: "fix", FixOp: fix.OpCheck, From: tg.Vol.Root, Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.(*fixAction).Verified; got != want || asked != m {
			t.Fatalf("manifest %q: verified %v (asked about %q)", m, got, asked)
		}
	}
}
