package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// Every action shows what it will do and what you'll get, and starts only after one "yes".
func TestConfirmShowsDetailsAndCanCancel(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	// Enter = continue at the "internal drive" note, then "n" at "Start now?"
	code, out, errOut := cli(t, true, "\nn\n", "rescue", "--from", src, "--to", dst, "--backup", "full")
	if code == 0 || copied(t, dst) {
		t.Fatalf("started after 'n': exit %d\n%s\n%s", code, out, errOut)
	}
	all := out + errOut
	for _, want := range []string{"From:", "To:", "Copy:", "Full", "You'll get:", "Reports", "Start now?", "nothing was started"} {
		if !strings.Contains(all, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, all)
		}
	}
}

func TestConfirmYesStarts(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	if code, out, errOut := cli(t, true, "\ny\n", "rescue", "--from", src, "--to", dst, "--backup", "full"); code != 0 || !copied(t, dst) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// A script confirms with --yes; without it nothing starts, even when there is no warning.
func TestNonTTYNeedsYesToConfirm(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "full", "--yes"); code != 0 {
		t.Fatalf("rescue: %s", errOut)
	}
	man := filepath.Join(dst, "Reports", "manifest.json")
	code, out, errOut := cli(t, false, "", "verify", "--manifest", man)
	if code == 0 || !strings.Contains(errOut, "--yes") || !strings.Contains(out, "You'll get:") {
		t.Fatalf("verify ran without confirmation: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := cli(t, false, "", "verify", "--manifest", man, "--yes"); code != 0 {
		t.Fatalf("verify --yes: %s", errOut)
	}
}

// No card or USB drive connected: say so, offer to look again, and allow an internal drive or a
// folder instead.
func TestPickerNoCardFound(t *testing.T) {
	old := listDrivesFn
	defer func() { listDrivesFn = old }()
	listDrivesFn = func() ([]drives.Drive, error) {
		return []drives.Drive{{Root: `C:\`, FS: "NTFS", Total: 500 << 30, System: true}}, nil
	}
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	// Enter = look again; 2 = type a folder; the folder; then Enter at the note and "y" to start
	in := "\n2\n" + src + "\n\ny\n"
	code, out, errOut := cli(t, true, in, "rescue", "--to", dst, "--backup", "full")
	if code != 0 || !copied(t, dst) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Count(errOut, "No card or USB drive was found") != 2 {
		t.Fatalf("not told twice (before and after looking again):\n%s", errOut)
	}
}

func TestNoFromNonTTYSaysNoCard(t *testing.T) {
	old := listDrivesFn
	defer func() { listDrivesFn = old }()
	listDrivesFn = func() ([]drives.Drive, error) { return []drives.Drive{{Root: `C:\`, System: true}}, nil }
	code, _, errOut := cli(t, false, "", "rescue", "--backup", "full", "--yes")
	if code != ExitUsage || !strings.Contains(errOut, "No card or USB drive was found") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}
