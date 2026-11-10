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
		return c.Run()
	}
	// OpenRaw opens the whole card for the health test, past the OS cache (rawrw_*.go).
	OpenRaw = openRaw
)

// HasRescue reports whether a volume root holds a rescue, recover or verify destination of ours
// (Reports/state.db or Reports/recover.db at the root or one folder down).
func HasRescue(root string) bool {
	if root == "" {
		return false
	}
	has := func(dir string) bool {
		for _, db := range []string{"state.db", "recover.db"} {
			if _, err := os.Stat(dir + string(os.PathSeparator) + "Reports" + string(os.PathSeparator) + db); err == nil {
				return true
			}
		}
		return false
	}
	if has(root) {
		return true
	}
	es, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for i, e := range es {
		if i > 5000 {
			break
		}
		if e.IsDir() && has(root+string(os.PathSeparator)+e.Name()) {
			return true
		}
	}
	return false
}
