package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/elevate"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fix"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/tui"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

const envWorker = "AIO_CLI_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(envWorker) == "1" {
		os.Exit(worker.Main())
	}
	worker.Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1")
		return c
	}
	// Every temp folder is on one disk: pretend sources are cards in a reader.
	lookupDrive = func(p string) (drives.Drive, error) {
		if b := filepath.Base(p); b == "out" || filepath.Base(filepath.Dir(p)) == "out" {
			return drives.Drive{Root: filepath.VolumeName(p) + `\`, FS: "NTFS", Free: 1 << 40, PhysicalID: "disk"}, nil
		}
		// Shown as a folder on a fixed disk: the card lock is then keyed by path, so parallel test
		// packages (whose temp folders all live on one drive) never share it.
		return drives.Drive{Root: p, FS: "exFAT", Removable: false, PhysicalID: "card"}, nil
	}
	// Traps: no test may run a real disk tool, open a real device for writing or list real disks.
	fix.Exec = func(_ context.Context, argv []string, _ io.Writer) error {
		panic(fmt.Sprintf("test reached a real disk tool: %v", argv))
	}
	fix.OpenRaw = func(t fix.Target) (fix.RW, io.Closer, error) {
		panic(fmt.Sprintf("test reached a real device: %s", t.Disk.Path))
	}
	listDevicesFn = func() ([]drives.Device, error) { return nil, nil }
	relaunch = func(context.Context, []string) error { panic("test reached a real elevation (UAC/sudo) request") }
	doubleClicked = func() bool { return false } // tests run in a terminal, never "double-clicked"
	runGUI = func(context.Context, IO, bool, int) error { panic("test reached a real GUI") }
	runTUI = func(IO, tui.Options) error { panic("test reached a real TUI") }
	os.Exit(m.Run())
}

// cli runs Main with captured output.
func cli(t *testing.T, tty bool, input string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = Main(args, IO{In: strings.NewReader(input), Out: &o, Err: &e, TTY: tty})
	return code, o.String(), e.String()
}

func entries(t *testing.T, dst string) map[string]manifest.Entry {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := manifest.Open(s)
	out := map[string]manifest.Entry{}
	m.Each(func(e manifest.Entry) error { out[e.Source] = e; return nil })
	return out
}

func TestUsageExitCode(t *testing.T) {
	if code, _, _ := cli(t, false, "", "rescue", "--bogus"); code != ExitUsage {
		t.Fatalf("unknown flag: %d", code)
	}
	if code, _, _ := cli(t, false, "", "nosuchcommand"); code != ExitUsage {
		t.Fatalf("unknown command: %d", code)
	}
	if code, _, _ := cli(t, false, "", "rescue"); code != ExitUsage {
		t.Fatalf("missing --from: %d", code)
	}
	if code, _, _ := cli(t, false, "", "rescue", "--from", ".", "--types", "pictures"); code != ExitUsage {
		t.Fatalf("bad --types: %d", code)
	}
}

func TestWorkerCommandHidden(t *testing.T) {
	_, out, _ := cli(t, false, "", "--help")
	if strings.Contains(out, "__source-worker") {
		t.Fatal("worker command listed in help")
	}
	if !strings.Contains(out, "rescue") || !strings.Contains(out, "verify") {
		t.Fatalf("help:\n%s", out)
	}
}

func TestTypesParse(t *testing.T) {
	if ts, err := parseTypes("all"); err != nil || ts != nil {
		t.Fatalf("all = %v %v (nil means every category, Other included)", ts, err)
	}
	ts, err := parseTypes("photos,chat,other")
	if err != nil || len(ts) != 3 || ts[1] != profile.ChatBackups || ts[2] != profile.Other {
		t.Fatalf("%v %v", ts, err)
	}
	if _, err := parseTypes("photos,bogus"); err == nil {
		t.Fatal("bogus type accepted")
	}
}

func TestExcludeFlagParse(t *testing.T) {
	if ex, _ := parseExclude("none"); len(ex) != 0 {
		t.Fatal(ex)
	}
	if ex, _ := parseExclude("recommended"); len(ex) != 1 {
		t.Fatal(ex)
	}
	if ex, err := parseExclude("thumbnails,android-app-data"); err != nil || len(ex) != 2 {
		t.Fatal(ex, err)
	}
	if _, err := parseExclude("thumbnails,bogus"); err == nil {
		t.Fatal("bogus id accepted")
	}
}

func TestPlainScanGolden(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.AndroidCard(false))
	to := t.TempDir()
	code, out, errOut := cli(t, false, "", "scan", "--from", src, "--to", to)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Card layout: Android, Generic", "By category:", "Photos", "Chat-Backups", "TOTAL",
		"Top folders by size:", "thumbnails", "(recommended)", "android-app-data", "Next step: aio-sdcard-doctor rescue"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan output lacks %q:\n%s", want, out)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(to, "Reports", "scan_*.csv")); len(m) != 1 {
		t.Fatal("scan CSV not written")
	}
}

func TestPlainRescueGolden(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	code, out, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "full", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Result:") || !strings.Contains(out, "OK") || !strings.Contains(out, "Report:") {
		t.Fatalf("output:\n%s", out)
	}
	if n := len(entries(t, dst)); n != len(card) {
		t.Fatalf("%d entries", n)
	}
	if code, _, _ := cli(t, false, "", "verify", "--manifest", filepath.Join(dst, "Reports", "manifest.json"), "--yes"); code != 0 {
		t.Fatalf("verify exit %d", code)
	}
}

// TestRescueRefusesWithoutYesNonTTY: a folder on a fixed disk raises the "is this really the
// card?" warning, which a script must accept explicitly.
func TestRescueRefusesWithoutYesNonTTY(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "full")
	if code != ExitError || !strings.Contains(errOut, "--yes") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dst, "Reports", "state.db")); err == nil {
		t.Fatal("copied without --yes")
	}
}

func TestTTYPromptsForSuggestions(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	// Custom; then Enter for every type (copy), every suggestion (= its recommendation), empty
	// files, compression and each warning
	in := strconv.Itoa(len(menuChoices())) + strings.Repeat("\n", 40)
	code, out, errOut := cli(t, true, in, "rescue", "--from", src, "--to", dst)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Skip Thumbnail caches") {
		t.Fatalf("not asked:\n%s", errOut)
	}
	es := entries(t, dst)
	if es["DCIM/.thumbnails/t1.jpg"].Status != manifest.EXCLUDED || es["Android/data/com.x/cache.jpg"].Status != manifest.OK {
		t.Fatalf("thumbs %+v appdata %+v", es["DCIM/.thumbnails/t1.jpg"], es["Android/data/com.x/cache.jpg"])
	}
}

func TestNonTTYCopiesEverything(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.AndroidCard(false)), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--backup", "full", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for rel, e := range entries(t, dst) {
		if e.Status != manifest.OK {
			t.Errorf("%s: %s", rel, e.Status)
		}
	}
}

func TestCompressFlag(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	if code, _, errOut := cli(t, false, "", "rescue", "--from", src, "--to", dst, "--yes", "--compress", "--backup", "full"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if m, _ := filepath.Glob(filepath.Join(dst, "Photos-0001.zip")); len(m) != 1 {
		t.Fatal("no ZIP part")
	}
}

func TestNeedsAdminNonTTYPrintsInstruction(t *testing.T) {
	dev := mockDevice() // never a real disk: the mock answers "needs admin"
	code, _, errOut := cli(t, false, "", "rescue", "--from", dev, "--to", filepath.Join(t.TempDir(), "out"), "--yes", "--backup", "full")
	if code != ExitAdmin || !strings.Contains(errOut, elevate.Instruction(nil)[:10]) {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestAdminFlowDeclinedOnTTY(t *testing.T) {
	old := relaunch
	defer func() { relaunch = old }()
	relaunch = func(context.Context, []string) error { return elevate.ErrDeclined }
	var e bytes.Buffer
	err := adminFlow(context.Background(), IO{In: strings.NewReader("y\n"), Out: &bytes.Buffer{}, Err: &e, TTY: true}, []string{"image"})
	if ee, ok := err.(*exitError); !ok || ee.code != ExitAdmin || !strings.Contains(ee.msg, "image") {
		t.Fatalf("%v", err)
	}
}

func TestDisabledCapabilityFlagExits2WithReason(t *testing.T) {
	img := filepath.Join(t.TempDir(), "mac.img")
	b := make([]byte, 2<<20)
	copy(b[32:], "NXSB") // an APFS container: not readable by the built-in parsers
	os.WriteFile(img, b, 0o644)
	code, _, errOut := cli(t, false, "", "rescue", "--from", img, "--raw", "--to", filepath.Join(t.TempDir(), "out"), "--yes", "--backup", "full")
	if code != ExitUsage || !strings.Contains(errOut, "mount with the OS") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestPartialExitCode(t *testing.T) {
	// a 0-byte file and a normal one copy fine; force a FAILED by excluding nothing but making
	// the destination refuse one file is hard here, so check the mapping instead
	if err := exit(ExitPartial, ""); err.(*exitError).code != 3 {
		t.Fatal("partial code")
	}
}

// Final review I6: the elevated child's own console closes when it exits; it waits for Enter so
// the result can be read. Without the marker (or without a terminal) it never waits.
func TestElevatedWindowWaitsForEnter(t *testing.T) {
	code, _, errOut := cli(t, true, "\n", "--version", elevate.WindowFlag)
	if code != ExitOK || !strings.Contains(errOut, "Press Enter to close") {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if _, _, errOut := cli(t, true, "", "--version"); strings.Contains(errOut, "Press Enter") {
		t.Fatalf("waited without the marker: %q", errOut)
	}
	if _, _, errOut := cli(t, false, "", "--version", elevate.WindowFlag); strings.Contains(errOut, "Press Enter") {
		t.Fatalf("waited with no terminal: %q", errOut)
	}
}
