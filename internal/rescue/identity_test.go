package rescue

import (
	"errors"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// Reading \\.\Q: directly, a destination on Q:\ is refused even when the physical disk can't be
// worked out (the guard must not fail open). Q: stands for a card; the fake destination is
// reported write-protected so that no test run can ever try to write anywhere real.
func TestRawVolumeDestOnSameVolumeRefused(t *testing.T) {
	old := physicalOf
	defer func() { physicalOf = old }()
	physicalOf = func(string) string { return "" }
	src := testutil.WriteFolder(t, testutil.CameraCard())
	r := newRescue(src, `Q:\rescue`, newFake(t, src))
	r.From, r.Raw = `\\.\Q:`, true
	r.LookupDrive = func(p string) (drives.Drive, error) {
		if p == r.From {
			return drives.Drive{}, errors.New("no volume")
		}
		return drives.Drive{Root: `Q:\`, FS: "exFAT", WriteProtected: true, Free: 1 << 40}, nil
	}
	if roots := r.sourceRoots(); len(roots) < 2 || roots[1] != `Q:\` {
		t.Fatalf("Q:\\ not protected: %v", roots)
	}
	blocked(t, r, "dest-inside-source")
}
