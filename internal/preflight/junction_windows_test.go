package preflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// junction makes link a directory junction to target (no admin rights needed).
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
}

// Adversarial pass S1: a destination reached through a junction (or subst, or a folder mount
// point) to the card passed every check, and the whole rescue was written onto the card.
func TestDestinationThroughJunctionIsOnTheCard(t *testing.T) {
	card := t.TempDir() // stands for the card's root
	link := filepath.Join(t.TempDir(), "CardLink")
	junction(t, link, card)
	in := base(t)
	in.Src.Root = card
	in.DstPath = filepath.Join(link, "Backup")
	expect(t, in, "dest-inside-source", job.Block)

	dest := filepath.Join(link, "Backup")
	os.MkdirAll(dest, 0o755)
	out := destfs.New(dest, card)
	defer func() {
		if recover() == nil {
			t.Error("the write guard let a file through the junction onto the card")
		}
	}()
	out.Create(filepath.Join(dest, "IMG_0001.JPG"))
}
