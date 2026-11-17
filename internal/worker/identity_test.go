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

// OA1: a card whose file system couldn't be read at the first open is known by its size and
// content samples; another card of the same size whose file system reads is known by its serial.
// Nothing was compared between the two, so after a respawn the stranger was read.
func TestRespawnRefusesAStrangerWhenOnlyOneHasASerial(t *testing.T) {
	const size = 64 << 20
	good, _ := os.ReadFile(testutil.FAT32Image(t, testutil.CameraCard(), size))
	unreadable := testutil.Random(size, 7)
	for name, order := range map[string][2][]byte{"unreadable, then a FAT card": {unreadable, good},
		"a FAT card, then an unreadable one": {good, unreadable}} {
		img := filepath.Join(t.TempDir(), "card.img")
		os.WriteFile(img, order[0], 0o644)
		c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
		r, _, cl, err := c.OpenDevice()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c.hook("Crash")
		os.WriteFile(img, order[1], 0o644) // another card, same size, in the same reader
		buf := make([]byte, 512)
		if _, err := r.ReadAt(buf, 0); !errors.Is(err, source.ErrDifferentCard) {
			t.Errorf("%s: read the stranger (err %v)", name, err)
		}
		cl.Close()
	}
}
