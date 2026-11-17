// Package fix is the only part of the program that writes to a card: check, repair and format with
// the OS's own tools, and a write/read health test that finds bad areas and fake-capacity cards.
// Every operation is behind Gate (which disk, which kind, rights, a verified rescue) and Confirm
// (the user types the card's label and size).
package fix

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// Operations.
const (
	OpCheck  = "check"       // read-only file system check
	OpRepair = "repair"      // fix the file system
	OpFormat = "format"      // erase and make a new file system
	OpHealth = "health-test" // write a pattern everywhere, read it back
)

// Target is what Fix acts on: the volume the user named (empty Root for a card with no
// mounted volume) and its physical disk with every volume on it.
type Target struct {
	Vol  drives.Drive
	Disk drives.Device
}

// Size is the card's size as the user sees it.
func (t Target) Size() int64 {
	if t.Disk.Size > 0 {
		return t.Disk.Size
	}
	return int64(t.Vol.Total)
}

// Refused is a hard no from Gate, with the reason to show.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "Fix refused: " + r.Reason }

// ErrNeedsAdmin: writing to a card needs administrator/root rights.
var ErrNeedsAdmin = errors.New("fixing a card needs administrator rights")

// Seams: tests replace them so nothing real is ever run or opened.
var (
	lookPath = exec.LookPath
	// Exec runs an OS tool, streaming its output.
	Exec = func(ctx context.Context, argv []string, out io.Writer) error {
		c := exec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Stdout, c.Stderr = out, out
		c.SysProcAttr = toolAttr()
		c.Cancel = stopTool(c, readOnlyTool(argv))
		err := c.Run()
		if err != nil && ctx.Err() != nil && !readOnlyTool(argv) && c.ProcessState != nil && c.ProcessState.Success() {
			// Stop came while a repair or format was left to finish, and it finished: exec reports
			// "canceled", but what the tool did is its exit code (OU2)
			return nil
		}
		return err
	}
	// OpenRaw opens the whole card for the health test, past the OS cache (rawrw_*.go).
	OpenRaw = openRaw
)

// rescueWalkFolders bounds HasRescue: folders listed, in all (a variable for tests). Files cost
// nothing, so a folder of 50,000 photos can't use it up.
var rescueWalkFolders = 5000

// rescueWalkTime bounds it in time: the walk reads the disk in our own process, and a dying card
// can take minutes per folder or never answer (OA7). A variable for tests.
var rescueWalkTime = 20 * time.Second

// readDir lists a folder (a seam for tests).
var readDir = os.ReadDir

// HasRescue reports whether a volume root holds something of ours worth keeping: a rescue,
// recover or verify destination (Reports/state.db, Reports/manifest.json or Reports/recover.db)
// or a card image (its .img.json / .img.map / .img.xz), at the root or up to six folders down.
// complete is false when the walk stopped before looking everywhere (its folder or time bound,
// a folder it couldn't list): "found nothing" then isn't "there is nothing".
func HasRescue(root string) (found, complete bool) {
	return hasRescue(context.Background(), root)
}

// hasRescue is HasRescue that also gives up on Stop. A read stuck on the card is left behind
// (it ends when the card answers or goes away); the answer is "couldn't look everywhere".
func hasRescue(ctx context.Context, root string) (found, complete bool) {
	if root == "" {
		return false, true
	}
	ctx, cancel := context.WithTimeout(ctx, rescueWalkTime)
	defer cancel()
	done := make(chan [2]bool, 1)
	list, budget := readDir, rescueWalkFolders // a walk left behind on a stuck card keeps its own
	go func() {
		f, c := walkForRescue(ctx, root, list, budget)
		done <- [2]bool{f, c}
	}()
	select {
	case r := <-done:
		return r[0], r[1]
	case <-ctx.Done():
		return false, false
	}
}

// walkForRescue looks breadth-first, so one big folder can't use up the budget before a rescue
// one level down is seen (OA6).
func walkForRescue(ctx context.Context, root string, readDir func(string) ([]os.DirEntry, error), budget int) (found, complete bool) {
	sep := string(os.PathSeparator)
	ours := func(dir string, es []os.DirEntry) bool {
		for _, e := range es {
			n := strings.ToLower(e.Name())
			if e.IsDir() && n == "reports" {
				for _, db := range []string{"state.db", "manifest.json", "recover.db"} {
					if _, err := os.Stat(dir + sep + e.Name() + sep + db); err == nil {
						return true
					}
				}
			}
			if !e.IsDir() && (strings.HasSuffix(n, ".img.json") || strings.HasSuffix(n, ".img.map") || strings.HasSuffix(n, ".img.xz")) {
				return true
			}
		}
		return false
	}
	type folder struct {
		dir   string
		depth int
	}
	start := strings.TrimRight(root, sep)
	if start == "" {
		start = root
	}
	queue := []folder{{start, 0}}
	complete = true
	for len(queue) > 0 {
		if budget--; budget < 0 || ctx.Err() != nil {
			return false, false
		}
		f := queue[0]
		queue = queue[1:]
		es, err := readDir(f.dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				complete = false // couldn't look in there: not "nothing there"
			}
			continue
		}
		if ours(f.dir, es) {
			return true, true
		}
		if f.depth == 6 {
			continue
		}
		for _, e := range es {
			if e.IsDir() && !strings.EqualFold(e.Name(), "Reports") {
				queue = append(queue, folder{f.dir + sep + e.Name(), f.depth + 1})
			}
		}
	}
	return false, complete
}

// stopTool is what Stop (or Ctrl+C) does to a running check, repair or format. A check changes
// nothing, so it is simply stopped. Killing a repair or format in the middle of writing the file
// system is the worst moment to stop it: on Windows it is left to finish (Stop waits for it);
// elsewhere it gets an interrupt, which fsck and mkfs handle.
func stopTool(c *exec.Cmd, readOnly bool) func() error {
	return func() error {
		if readOnly {
			return c.Process.Kill()
		}
		if runtime.GOOS == "windows" {
			return nil
		}
		return c.Process.Signal(os.Interrupt)
	}
}

// readOnlyTool: the command line only checks (chkdsk without switches, diskutil verifyVolume,
// fsck -n), so stopping it can't hurt the card.
func readOnlyTool(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch tool := strings.ToLower(argv[0]); {
	case tool == "chkdsk":
		for _, a := range argv[1:] {
			if strings.HasPrefix(a, "/") { // /f, /x, /r … change the card
				return false
			}
		}
		return true
	case tool == "diskutil":
		return len(argv) > 1 && argv[1] == "verifyVolume"
	case strings.HasPrefix(tool, "fsck") || tool == "e2fsck":
		return slices.Contains(argv[1:], "-n")
	}
	return false
}
