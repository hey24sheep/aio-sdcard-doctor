package destfs

import (
	"path/filepath"
	"runtime"
	"testing"
)

// SU1 (adv3): a pasted `"D:\rescue"` (with its quotes), `aux` or `a:b` as the destination made
// the write guard panic and took the whole window down; CheckRoot says so as an error first.
func TestCheckRootRefusesPathsTheGuardCannotUse(t *testing.T) {
	dir := t.TempDir()
	if err := CheckRoot(filepath.Join(dir, "rescue")); err != nil {
		t.Fatalf("plain folder refused: %v", err)
	}
	bad := []string{"", `"` + filepath.Join(dir, "dq") + `"`}
	if runtime.GOOS == "windows" {
		bad = append(bad, filepath.Join(dir, "aux"), filepath.Join(dir, "con.txt"), filepath.Join(dir, "a:b"), `\\.\PhysicalDrive1`)
	} else {
		bad = append(bad, "/dev/sdz")
	}
	for _, p := range bad {
		if err := CheckRoot(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
