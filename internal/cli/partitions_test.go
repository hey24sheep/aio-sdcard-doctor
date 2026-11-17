package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The "more partitions" note now comes from rescue's plan (so the GUI and TUI show it too); a
// partition picked with --volume must still silence it, and it is said once, not twice.
func TestRescuePartitionNoteOnceAndSilencedByVolume(t *testing.T) {
	img := twoPartitionImage(t)
	code, out, errOut := cli(t, false, "", "rescue", "--from", img, "--to", filepath.Join(t.TempDir(), "out"), "--backup", "full", "--yes")
	// OC8: the same --to folder is refused as another card's rescue, so the note says "a new folder"
	if code != 0 || strings.Count(out+errOut, "2 partitions") != 1 || !strings.Contains(out+errOut, "new folder") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = cli(t, false, "", "rescue", "--from", img, "--to", filepath.Join(t.TempDir(), "out"), "--backup", "full", "--yes", "--volume", "1")
	if code != 0 || strings.Contains(out+errOut, "2 partitions") {
		t.Fatalf("--volume 1: exit %d\n%s\n%s", code, out, errOut)
	}
}
