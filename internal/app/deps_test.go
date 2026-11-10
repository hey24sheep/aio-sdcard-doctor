package app

import (
	"context"
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
	tg := fix.Target{Vol: drives.Drive{Root: `Q:\`, Label: "CAMERA", FS: "FAT32", Total: 16 << 30, Removable: true},
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
