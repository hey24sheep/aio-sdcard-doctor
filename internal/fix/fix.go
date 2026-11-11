// Package fix is the only part of the program that writes to a card: check, repair and format with
// the OS's own tools, and a write/read health test that finds bad areas and fake-capacity cards.
// Every operation is behind Gate (which disk, which kind, rights, a verified rescue) and Confirm
// (the user types the card's label and size).
package fix

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

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
		c.Cancel = stopTool(c)
		return c.Run()
	}
	// OpenRaw opens the whole card for the health test, past the OS cache (rawrw_*.go).
	OpenRaw = openRaw
)

// HasRescue reports whether a volume root holds something of ours worth keeping: a rescue,
// recover or verify destination (Reports/state.db or Reports/recover.db) or a card image (its
// .img.json / .img.map / .img.xz), at the root or up to three folders down. The walk is bounded.
func HasRescue(root string) bool {
	if root == "" {
		return false
	}
	sep := string(os.PathSeparator)
	ours := func(dir string, es []os.DirEntry) bool {
		for _, db := range []string{"state.db", "recover.db"} {
			if _, err := os.Stat(dir + sep + "Reports" + sep + db); err == nil {
				return true
			}
		}
		for _, e := range es {
			n := strings.ToLower(e.Name())
			if !e.IsDir() && (strings.HasSuffix(n, ".img.json") || strings.HasSuffix(n, ".img.map") || strings.HasSuffix(n, ".img.xz")) {
				return true
			}
		}
		return false
	}
	budget := 20000 // folder entries looked at, in all
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		es, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		if ours(dir, es) {
			return true
		}
		if depth == 3 {
			return false
		}
		for _, e := range es {
			if budget--; budget < 0 {
				return false
			}
			if e.IsDir() && e.Name() != "Reports" && walk(dir+sep+e.Name(), depth+1) {
				return true
			}
		}
		return false
	}
	return walk(strings.TrimRight(root, sep), 0)
}

// stopTool is what Stop (or Ctrl+C) does to a running check, repair or format. Killing one in the
// middle of writing the file system is the worst moment to stop it: on Windows it is left to
// finish (Stop waits for it); elsewhere it gets an interrupt, which fsck and mkfs handle.
func stopTool(c *exec.Cmd) func() error {
	return func() error {
		if runtime.GOOS == "windows" {
			return nil
		}
		return c.Process.Signal(os.Interrupt)
	}
}
