package rescue

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

const camJPG = "DCIM/100CANON/IMG_0001.JPG"

// Adversarial pass C1: ZIP mode trusted its index alone. With the ZIP parts deleted (say after
// verify flagged one), a re-run said "already copied" and Start over wrote nothing.
func TestCompressDeletedPartsAreCopiedAgain(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	do(t, r, nil, nil)
	parts, _ := filepath.Glob(filepath.Join(dst, "*.zip"))
	if len(parts) == 0 {
		t.Fatal("no ZIP parts written")
	}
	for _, p := range parts {
		os.Remove(p)
	}
	r = newRescue(src, dst, newFake(t, src))
	r.Compress = true
	res, err := do(t, r, nil, nil)
	if err != nil || res.Counters["ALREADY-COPIED"] != 0 {
		t.Fatalf("parts gone, yet: %v %v", res.Counters, err)
	}
	if parts, _ := filepath.Glob(filepath.Join(dst, "*.zip")); len(parts) == 0 {
		t.Fatal("nothing written again")
	}
}

// Adversarial pass C2: a fresh copy of equal quality never replaced the recorded one, so Start
// over couldn't repair a damaged copy ...
func TestStartOverRepairsDamagedCopy(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	p := filepath.Join(dst, filepath.FromSlash(entries(t, dst)[camJPG].Dest))
	b, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Repeat([]byte{0}, len(b)), 0o644) // same size, wrong content
	r := newRescue(src, dst, newFake(t, src))
	r.Fresh = true
	do(t, r, nil, nil)
	if got, _ := os.ReadFile(p); !bytes.Equal(got, card[camJPG].Data) {
		t.Fatal("Start over left the damaged copy in place")
	}
}

// ... and a file that changed on the card since the last run kept its old copy, reported OK.
func TestChangedCardFileIsCopiedAgain(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	newer := append(append([]byte{}, card[camJPG].Data...), []byte("more data")...)
	sp := filepath.Join(src, filepath.FromSlash(camJPG))
	os.WriteFile(sp, newer, 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(sp, later, later)
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	e := entries(t, dst)[camJPG]
	got, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.Dest)))
	if !bytes.Equal(got, newer) || e.Bytes != int64(len(newer)) {
		t.Fatalf("stale copy kept: %d bytes on disk, manifest %+v", len(got), e)
	}
}

// Adversarial pass S2: a hub reset dropped the card and the destination disk, and they came back
// with swapped letters (or "NO NAME" / "NO NAME 1" on macOS): the card is now where the
// destination was. The guard still protected only the card's old path, and the next files went
// onto the card. The run must stop instead.
func TestCardBackWhereTheDestinationWasStops(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	r := newRescue(src, dst, f)
	r.Present = func(drives.Identity, string) (string, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.gone {
			return f.root, true
		}
		return filepath.Dir(dst), true // the card's new mount point holds the destination folder
	}
	calls := 0
	f.opt.OnRead = func(off int64, n int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if calls++; calls == 5 && f.retargets == 0 {
			f.gone = true
		}
		if f.gone && f.retargets == 0 {
			return errors.New("device not ready")
		}
		return nil
	}
	_, err := do(t, r, &prompter{}, nil)
	if err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("kept copying onto the card: %v", err)
	}
}

// Adversarial pass S12: the card lock was taken only when copying started, after the plan had
// already scanned the card, so two windows could read the same failing card at once. The plan
// takes it; a second window is told the card is busy.
func TestPlanTakesTheCardLock(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.CameraCard())
	f := newFake(t, src)
	a := newRescue(src, filepath.Join(t.TempDir(), "a"), f)
	defer a.Close()
	if _, err := a.Plan(context.Background(), job.Env{Prompt: &prompter{}}); err != nil {
		t.Fatal(err)
	}
	b := newRescue(src, filepath.Join(t.TempDir(), "b"), f)
	defer b.Close()
	p, err := b.Plan(context.Background(), job.Env{Prompt: &prompter{}})
	busy := errors.Is(err, job.ErrSourceBusy)
	for _, is := range p.Issues {
		busy = busy || is.Kind == "busy"
	}
	if !busy {
		t.Fatalf("a second window planned the same card: %v %+v", err, p.Issues)
	}
	a.Close()
	b.Close()
	if _, err := b.Plan(context.Background(), job.Env{Prompt: &prompter{}}); err != nil {
		t.Fatalf("lock not released by Close: %v", err)
	}
}

// Adversarial pass C5: after a retry round the summary counted a file twice (OK 5 and STALLED 1
// for a 5-file card) because only the counters were corrected, not the summary.
func TestSummaryAfterRetryCountsOnce(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.openErr[camJPG] = []error{source.ErrStalled, nil}
	if _, err := do(t, newRescue(src, dst, f), nil, nil); err != nil {
		t.Fatal(err)
	}
	sums, _ := filepath.Glob(filepath.Join(dst, "Reports", "copy_summary_*.txt"))
	if len(sums) != 1 {
		t.Fatalf("summaries %v", sums)
	}
	if b, _ := os.ReadFile(sums[0]); strings.Contains(string(b), "STALLED") {
		t.Fatalf("a file retried and copied still counted as stalled:\n%s", b)
	}
}
