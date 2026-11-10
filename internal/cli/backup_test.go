package cli

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

func TestRescueNonTTYNeedsBackupChoice(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--yes")
	if code != ExitUsage {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"--backup", "quick", "standard", "full", "whatsapp", "telegram", "--types"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("message lacks %q:\n%s", want, errOut)
		}
	}
}

func TestBackupPresetFlag(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	code, out, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "quick", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	es := entries(t, dst)
	if es["DCIM/Camera/IMG_20240101_101010.jpg"].Status != manifest.OK || es["DCIM/Camera/VID_20240103.mp4"].Status != manifest.EXCLUDED {
		t.Fatalf("photo %+v video %+v", es["DCIM/Camera/IMG_20240101_101010.jpg"], es["DCIM/Camera/VID_20240103.mp4"])
	}
	if !strings.Contains(out, "Backup: Quick") {
		t.Fatalf("output lacks the choice:\n%s", out)
	}
}

func TestBackupMixedWithTypesIsUsageError(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.CameraCard())
	if code, _, _ := cli(t, false, "", "rescue", "--from", src, "--backup", "quick", "--types", "photos"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if code, _, _ := cli(t, false, "", "rescue", "--from", src, "--backup", "nonsense"); code != ExitUsage {
		t.Fatalf("unknown preset: exit %d", code)
	}
}

func TestCustomTypesWithoutBackup(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--types", "photos", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if e := entries(t, dst)["Music/song.mp3"]; e.Status != manifest.EXCLUDED {
		t.Fatalf("%+v", e)
	}
}

func TestSkipEmptyFlag(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "full", "--skip-empty", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if e := entries(t, dst)["Download/empty.pdf"]; e.Status != manifest.EXCLUDED || !strings.Contains(e.Error, "0 bytes") {
		t.Fatalf("%+v", e)
	}
}

func TestAppsFlag(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--apps", "telegram", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	ok := 0
	for _, e := range entries(t, dst) {
		if e.Status == manifest.OK {
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("%d files copied for telegram only", ok)
	}
}

// TestTTYBackupMenu: the first question is the backup choice, with what each would copy.
func TestTTYBackupMenu(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	// 1 = Quick; then Enter for "empty files?" (copy), Enter for "compress?" (no), Enter for warnings
	code, out, errOut := cli(t, true, "1\n"+strings.Repeat("\n", 10), "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"What should be copied?", "Quick", "Standard", "Full", "WhatsApp only", "Custom", "files", "Compress"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("menu lacks %q:\n%s", want, errOut)
		}
	}
	es := entries(t, dst)
	if es["DCIM/Camera/VID_20240103.mp4"].Status != manifest.EXCLUDED || es["Docs/report.pdf"].Status != manifest.OK {
		t.Fatal("Quick not applied")
	}
}

// TestTTYCustomMenu: Custom asks per type (only types on the card) and per skip suggestion.
func TestTTYCustomMenu(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	custom := len(menuChoices())
	// custom; Photos yes, every other type no; Enter for the rest (suggestions, empty, compress, warnings)
	in := strconv.Itoa(custom) + "\ny\n" + strings.Repeat("n\n", 6) + strings.Repeat("\n", 12)
	code, out, errOut := cli(t, true, in, "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	es := entries(t, dst)
	if es["DCIM/Camera/IMG_20240101_101010.jpg"].Status != manifest.OK || es["Music/song.mp3"].Status != manifest.EXCLUDED {
		t.Fatalf("custom not applied:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Skip Thumbnail caches") {
		t.Fatal("custom didn't ask about skip suggestions")
	}
}
