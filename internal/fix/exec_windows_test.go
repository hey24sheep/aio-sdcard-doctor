//go:build windows

package fix

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// OD3/SA-6: the windowed build has no console, so chkdsk and format got a console window of
// their own; closing it (or Ctrl+C in ours) killed them mid-write.
func TestToolsGetNoWindowAndTheirOwnGroup(t *testing.T) {
	a := toolAttr()
	if a == nil || !a.HideWindow || a.CreationFlags&windows.CREATE_NO_WINDOW == 0 || a.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("attr %+v", a)
	}
	var out bytes.Buffer // the tool's output still reaches us
	if err := Exec(context.Background(), []string{"cmd", "/c", "echo", "tool output"}, &out); err != nil || !strings.Contains(out.String(), "tool output") {
		t.Fatalf("err %v out %q", err, out.String())
	}
}

// OU2/OA10: a repair or format is left to finish when Stop is pressed; exec then reported
// "context canceled" though the tool exited 0 — the card was repaired or formatted, and the
// user was told it was stopped.
func TestStoppedToolThatFinishedReportsItsExitCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	var out bytes.Buffer
	if err := Exec(ctx, []string{"cmd", "/c", "ping -n 2 127.0.0.1 >nul && echo finished"}, &out); err != nil {
		t.Fatalf("err %v (out %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "finished") {
		t.Fatalf("the tool didn't run to its end: %q", out.String())
	}
}
