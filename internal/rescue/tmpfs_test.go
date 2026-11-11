package rescue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// TestDiskFullTmpfs fills a real, tiny file system (CI mounts a 4 MiB tmpfs at AIO_TMPFS): the
// OS's own "no space left" mid-copy is asked about, the run stops with an error, and no
// half-written file is left behind. Free space is overstated so the run starts at all.
func TestDiskFullTmpfs(t *testing.T) {
	dir := os.Getenv("AIO_TMPFS")
	if dir == "" {
		t.Skip("set AIO_TMPFS to a small empty file system (CI: a 4 MiB tmpfs)")
	}
	card := testutil.Card{}
	for i, name := range []string{"DCIM/A.JPG", "DCIM/B.JPG", "DCIM/C.JPG"} {
		card[name] = testutil.File{Data: testutil.Random(2<<20, byte(i+1))}
	}
	src := testutil.WriteFolder(t, card)
	dst, err := os.MkdirTemp(dir, "out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dst) })
	r := newRescue(src, dst, newFake(t, src))
	r.Free = func(string) (uint64, error) { return 1 << 40, nil }
	p := &prompter{}
	_, err = do(t, r, p, nil)
	if err == nil || p.asked["disk-full"] != 1 {
		t.Fatalf("err=%v asked=%v", err, p.asked)
	}
	noTmp(t, dst)
	if _, err := os.Stat(filepath.Join(dst, "Photos", "DCIM", "C.JPG")); err == nil {
		t.Fatal("a file that didn't fit is on the destination")
	}
}
