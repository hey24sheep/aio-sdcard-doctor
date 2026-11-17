//go:build linux

package fix

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// SA-6: "unmounted" was decided at Plan; a card mounted again before the health test opened it
// (an automounter) was written under its file system. O_EXCL makes the kernel refuse a block
// device any partition of which is mounted, and the refusal says what to do.
func TestHealthTestOpensTheCardExclusively(t *testing.T) {
	if openFlags()&unix.O_EXCL == 0 {
		t.Fatal("no O_EXCL")
	}
	old := openFile
	defer func() { openFile = old }()
	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: "/dev/sdb", Err: unix.EBUSY}
	}
	tg := card(16 << 30)
	tg.Disk.Path, tg.Disk.Volumes = "/dev/sdb", nil
	if _, _, err := openRaw(tg); err == nil || !strings.Contains(err.Error(), "unmount it first") {
		t.Fatalf("err %v", err)
	}
}
