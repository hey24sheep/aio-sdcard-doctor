package worker

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// After a re-plug or a respawn the worker must be reading the same card, never a stranger that
// appeared at the same path.
func TestRetargetToDifferentCardRefused(t *testing.T) {
	f, _ := os.Open(filepath.Join("..", "fsread", "testdata", "ntfs.img.gz"))
	defer f.Close()
	z, _ := gzip.NewReader(f)
	b, _ := io.ReadAll(z)
	ntfs := filepath.Join(t.TempDir(), "ntfs.img")
	os.WriteFile(ntfs, b, 0o644)
	fat := testutil.FAT32Image(t, testutil.CameraCard(), 64<<20)

	c := spawn(t, Spec{From: ntfs, Raw: true}, 5*time.Second)
	if err := c.Retarget(Spec{From: fat, Raw: true}); !errors.Is(err, source.ErrDifferentCard) {
		t.Fatalf("err = %v", err)
	}
}
