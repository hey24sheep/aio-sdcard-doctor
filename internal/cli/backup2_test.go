package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// #3: a trailing comma never turns a type list into "everything".
func TestTypesTrailingCommaNotAll(t *testing.T) {
	ts, err := parseTypes("photos,")
	if err != nil || len(ts) != 1 {
		t.Fatalf("%v %v", ts, err)
	}
	if _, err := parseTypes(","); err == nil {
		t.Fatal("empty type list accepted")
	}
}

func copied(t *testing.T, dst string) bool {
	_, err := os.Stat(filepath.Join(dst, "Reports", "state.db"))
	return err == nil
}

// #4: --skip-empty survives the menu.
func TestSkipEmptyFlagKeptThroughMenu(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	full := strconv.Itoa(3)
	if code, out, errOut := cli(t, true, full+"\n"+strings.Repeat("\n", 10), "rescue", "--from", src, "--to", dst, "--skip-empty"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if e := entries(t, dst)["Download/empty.pdf"]; e.Status != manifest.EXCLUDED {
		t.Fatalf("%+v", e)
	}
}

// #5: no answer is never a choice.
func TestMenuNeverChoosesForTheUser(t *testing.T) {
	for name, in := range map[string]string{"eof": "", "bad input then eof": "x\nyes\n99\n"} {
		src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
		code, _, errOut := cli(t, true, in, "rescue", "--from", src, "--to", dst)
		if code == 0 || copied(t, dst) {
			t.Fatalf("%s: exit %d, copied=%v\n%s", name, code, copied(t, dst), errOut)
		}
	}
}

// #5: Custom with nothing ticked asks again instead of copying everything.
func TestCustomNothingTickedAsksAgain(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	custom := strconv.Itoa(len(menuChoices()))
	// first round: no to all 7 types; second round: yes to Photos, no to the rest; then Enter
	in := custom + "\n" + strings.Repeat("n\n", 7) + "y\n" + strings.Repeat("n\n", 6) + strings.Repeat("\n", 15)
	code, out, errOut := cli(t, true, in, "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Nothing selected") {
		t.Fatalf("not re-asked:\n%s", errOut)
	}
	if e := entries(t, dst)["Music/song.mp3"]; e.Status != manifest.EXCLUDED {
		t.Fatalf("music %+v", e)
	}
}

// #6: a re-run offers the previous choice; Enter keeps it.
func TestRerunOffersPreviousChoice(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "quick", "--yes"); code != 0 {
		t.Fatalf("first run: %s", errOut)
	}
	code, out, errOut := cli(t, true, strings.Repeat("\n", 12), "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "same choice as last time (Quick") {
		t.Fatalf("previous choice not offered:\n%s", errOut)
	}
	if e := entries(t, dst)["DCIM/Camera/VID_20240103.mp4"]; e.Status != manifest.EXCLUDED {
		t.Fatalf("video %+v", e)
	}
}

// #9: no menu (with zero counts) before asking for admin rights.
func TestNoMenuBeforeElevation(t *testing.T) {
	dev := mockDevice() // never a real disk: the mock answers "needs admin"
	oldAdmin := isAdmin
	defer func() { isAdmin = oldAdmin }()
	isAdmin = func() bool { return false } // CI runners run as Administrator
	code, _, errOut := cli(t, true, "n\n", "rescue", "--from", dev, "--to", filepath.Join(t.TempDir(), "out"))
	if code != ExitAdmin || strings.Contains(errOut, "What should be copied?") {
		t.Fatalf("exit %d:\n%s", code, errOut)
	}
}

// #13: Custom asks about apps too.
func TestCustomAsksApps(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	custom := strconv.Itoa(len(menuChoices()))
	// every type yes, apps: 3 = Telegram, then Enter for the rest
	in := custom + "\n" + strings.Repeat("y\n", 7) + "3\n" + strings.Repeat("\n", 15)
	code, out, errOut := cli(t, true, in, "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	ok := 0
	for _, e := range entries(t, dst) {
		if e.Status == manifest.OK {
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("%d files copied for Telegram only\n%s", ok, errOut)
	}
}
