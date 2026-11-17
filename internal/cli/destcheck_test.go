package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// SU1 (CLI side): scan --to with a path the write guard can't use (quotes left in it) printed a
// Go stack; it is a usage error that says why.
func TestScanToUnusableFolderIsAUsageError(t *testing.T) {
	card := testutil.WriteFolder(t, testutil.CameraCard())
	bad := `"` + filepath.Join(t.TempDir(), "out") + `"`
	code, _, errOut := cli(t, false, "", "scan", "--from", card, "--to", bad)
	if code != ExitUsage || !strings.Contains(errOut, "quotes") || strings.Contains(errOut, "goroutine") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestRescueRecoverImageToUnusableFolderIsAUsageError(t *testing.T) {
	card := testutil.WriteFolder(t, testutil.CameraCard())
	bad := `"` + filepath.Join(t.TempDir(), "out") + `"`
	for name, args := range map[string][]string{
		"rescue":  {"rescue", "--from", card, "--to", bad, "--backup", "full", "--yes"},
		"recover": {"recover", "--from", card, "--to", bad, "--yes"},
		"image":   {"image", "--from", card, "--to", filepath.Join(bad, "card.img"), "--yes"},
	} {
		code, _, errOut := cli(t, false, "", args...)
		if code != ExitUsage || strings.Contains(errOut, "goroutine") {
			t.Errorf("%s: exit %d: %s", name, code, errOut)
		}
	}
}
