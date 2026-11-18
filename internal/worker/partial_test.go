package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/scan"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// partialCard is a FAT32 image whose DCIM folder has one sector of its listing zeroed (as an image
// of a card with an unreadable sector there holds): want is how many DCIM files are still listed.
func partialCard(t *testing.T) (img string, want int) {
	t.Helper()
	card := testutil.Card{}
	for i := 0; i < 300; i++ {
		card[fmt.Sprintf("DCIM/F%04d.JPG", i)] = testutil.File{Data: testutil.JPEG(100, fmt.Sprint(i))}
	}
	p := testutil.FAT32Image(t, card, 64<<20)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// DCIM's first sector (its "." record): files are written in map order, so a named file can
	// land in the listing's last sector, where zeros look exactly like the folder's normal end
	at := bytes.Index(b, []byte(".          \x10"))
	if at < 0 {
		t.Fatal("no \".\" record for DCIM")
	}
	sec := at &^ 511
	clear(b[sec : sec+512])
	img = filepath.Join(t.TempDir(), "card.img")
	if err := os.WriteFile(img, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// what the parser itself lists, read in this process
	vs, err := fsread.Probe(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	src, err := fsread.Open(bytes.NewReader(b), vs[0])
	if err != nil {
		t.Fatal(err)
	}
	es, err := fs.ReadDir(src, "DCIM")
	if !errors.Is(err, fsread.ErrPartialFolder) || len(es) == 0 || len(es) >= 300 {
		t.Fatalf("fixture: %d entries, err %v (want a partial listing)", len(es), err)
	}
	return img, len(es)
}

// OR1: a folder that can be read only in part lists its readable entries with an error saying
// so. Over the worker the error came back alone: net/rpc drops the reply of a call that fails,
// so the readable files were lost too.
func TestPartialFolderThroughTheWorker(t *testing.T) {
	img, want := partialCard(t)
	c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
	es, err := c.ReadDir("DCIM")
	if !errors.Is(err, fsread.ErrPartialFolder) {
		t.Fatalf("err %v, want a partial-folder error", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != "DCIM" {
		t.Fatalf("err %v doesn't name the folder", err)
	}
	if len(es) != want {
		t.Fatalf("%d entries, want the %d readable ones", len(es), want)
	}
	// a scan through the worker lists the readable files and names the folder as not fully listed
	inv, err := scan.Scan(context.Background(), c, profile.Detect(c))
	if err != nil {
		t.Fatal(err)
	}
	named := slices.ContainsFunc(inv.Unlistable, func(u string) bool { return strings.HasPrefix(u, "DCIM") })
	if len(inv.Items) != want || !named {
		t.Fatalf("scan: %d files (want %d), unlistable %v", len(inv.Items), want, inv.Unlistable)
	}
}

// OR1 for recover: a check of a found file that hit a read error (bad sectors) is not the same
// as a file that is corrupt; the worker says which.
func TestValidateCheckedSaysWhenAReadFailed(t *testing.T) {
	img := testutil.FAT32Image(t, testutil.CameraCard(), 64<<20)
	c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
	ext := [][2]int64{{1 << 20, 64 << 10}}
	if _, _, failed, err := c.ValidateChecked("jpg", 0, ext, 64<<10); err != nil || failed {
		t.Fatalf("readable: failed %v err %v", failed, err)
	}
	if err := c.hook(fmt.Sprintf("BadArea:%d:%d", 1<<20, 2<<20)); err != nil {
		t.Fatal(err)
	}
	ok, _, failed, err := c.ValidateChecked("jpg", 0, ext, 64<<10)
	if err != nil || ok || !failed {
		t.Fatalf("unreadable: ok %v failed %v err %v", ok, failed, err)
	}
}
