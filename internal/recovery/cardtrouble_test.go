package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// Adversarial pass 3.

// stopsResponding reads fine until gone is set, then answers like a worker that gave up.
type stopsResponding struct {
	r     io.ReaderAt
	gone  *atomic.Bool
	after *atomic.Int64 // reads tried while gone
}

func (s stopsResponding) ReadAt(p []byte, off int64) (int, error) {
	if s.gone.Load() {
		s.after.Add(1)
		return 0, source.ErrTooManyRestarts
	}
	return s.r.ReadAt(p, off)
}

// answers records the questions asked and answers them with reply.
type answers struct {
	asked []string
	reply func(job.Issue) job.Choice
}

func (a *answers) Ask(_ context.Context, is job.Issue) job.Choice {
	a.asked = append(a.asked, is.Kind)
	return a.reply(is)
}

// photosCard is a raw card of 16 MiB with five photos a deep scan finds.
func photosCard(t *testing.T) []byte {
	img := make([]byte, 16<<20)
	for i := 0; i < 5; i++ {
		copy(img[(1+3*i)<<20:], realJPEG(t, 300, 200, byte(10+i)))
	}
	return img
}

func fakeRecover(t *testing.T, dev io.ReaderAt, size int64) *Recover {
	r := &Recover{From: filepath.Join(t.TempDir(), "card.img"), Dest: filepath.Join(t.TempDir(), "out"), Deep: true,
		OpenSource: func(worker.Spec, time.Duration) (Source, error) {
			return &fakeSrc{dev: dev, size: size}, nil
		}}
	os.WriteFile(r.From, nil, 0o644)
	return r
}

// OR4: a card that stopped responding while files were recovered had every remaining find
// written down as damaged ("N bytes could not be read") and the run ended as if done. Recover
// asks, as rescue does: Wait (the run goes on with the card) or Stop.
func TestCardStoppingDuringRecoverAsks(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		img := photosCard(t)
		var gone atomic.Bool
		var after atomic.Int64
		r := fakeRecover(t, stopsResponding{bytes.NewReader(img), &gone, &after}, int64(len(img)))
		defer r.Close()
		p := plan(t, r)
		e := env()
		a := &answers{reply: func(job.Issue) job.Choice { return job.Stop }}
		e.Prompt = a
		e.Progress = func(p job.Progress) {
			if p.Phase == "Recovering" {
				gone.Store(true)
			}
		}
		_, err := r.Run(context.Background(), e, p)
		if err == nil || !strings.Contains(err.Error(), "stopped responding") {
			t.Errorf("Run: %v, want it stopped because the card stopped responding", err)
		}
		if strings.Join(a.asked, ",") != "card-not-responding" {
			t.Errorf("asked %v, want one card-not-responding question", a.asked)
		}
		for k, en := range entries(t, r.Dest) {
			if en.Status == manifest.CARVED_DAMAGED {
				t.Errorf("%s written down as damaged: %s", k, en.Error)
			}
		}
	})
	t.Run("wait", func(t *testing.T) {
		img := photosCard(t)
		var gone atomic.Bool
		var after atomic.Int64
		r := fakeRecover(t, stopsResponding{bytes.NewReader(img), &gone, &after}, int64(len(img)))
		defer r.Close()
		p := plan(t, r)
		e := env()
		once := false
		e.Progress = func(p job.Progress) {
			if p.Phase == "Recovering" && !once {
				once = true
				gone.Store(true)
			}
		}
		e.Prompt = &answers{reply: func(job.Issue) job.Choice { gone.Store(false); return job.Wait }} // the card is back
		res, err := r.Run(context.Background(), e, p)
		if err != nil {
			t.Fatal(err)
		}
		if res.Counters[string(manifest.CARVED)] != 5 {
			t.Errorf("counters %v, want all five photos recovered", res.Counters)
		}
	})
}

// SR9: a file that failed its check was read again in full just to count its unreadable bytes.
// The count is capped, and says "at least".
func TestUnreadableCountIsCapped(t *testing.T) {
	var reads atomic.Int64
	r := countAll{&reads}
	n, all, err := unreadable(context.Background(), r, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if read := reads.Load(); read > maxRecount+(1<<20) {
		t.Errorf("read %d MiB again to count a 1 GiB file's bad bytes", read>>20)
	}
	if all || n == 0 {
		t.Errorf("counted %d (whole file %v), want a part of it", n, all)
	}
}

// countAll fails every read and counts the bytes asked for.
type countAll struct{ n *atomic.Int64 }

func (c countAll) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(int64(len(p)))
	return 0, errors.New("I/O error")
}

// unreadableDeleted is a card whose deleted-file list can't be read (a damaged FAT): Deleted fails.
type unreadableDeleted struct{ fakeSrc }

func (u *unreadableDeleted) ReadError() string { return "" }
func (u *unreadableDeleted) Caps() []job.Capability {
	return []job.Capability{{Name: "Undelete", Enabled: true}}
}
func (u *unreadableDeleted) Deleted(context.Context, func(source.Deleted) error) error {
	return errors.New("invalid cluster: 0")
}

// SR1: a deleted-file list that couldn't be read stopped the whole recovery, deep scan included.
// The deep scan goes on, with a warning.
func TestDeepScanGoesOnWhenDeletedListFails(t *testing.T) {
	img := photosCard(t)
	r := fakeRecover(t, bytes.NewReader(img), int64(len(img)))
	r.OpenSource = func(worker.Spec, time.Duration) (Source, error) {
		return &unreadableDeleted{fakeSrc{dev: bytes.NewReader(img), size: int64(len(img))}}, nil
	}
	defer r.Close()
	p := plan(t, r)
	var warned []string
	e := env()
	e.Log = func(level, msg string) {
		if level == "warn" {
			warned = append(warned, msg)
		}
	}
	res, err := r.Run(context.Background(), e, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters[string(manifest.CARVED)] != 5 {
		t.Errorf("counters %v, want the five photos found by the deep scan", res.Counters)
	}
	if len(warned) == 0 || !strings.Contains(strings.Join(warned, "\n"), "invalid cluster") {
		t.Errorf("warnings %q, want the list's error", warned)
	}
}

// SR8: an unreadable area is stored as soon as it is found, but the scan position only at a
// checkpoint; after a crash in between, the resumed scan listed the area twice.
func TestUnreadableAreaListedOnceAfterACrash(t *testing.T) {
	img := photosCard(t)
	dev := badAt{r: bytes.NewReader(img), from: 8 << 20, to: 8<<20 + 512}
	r := fakeRecover(t, dev, int64(len(img)))
	r.List = true
	run(t, r)
	st, err := job.OpenStore(r.storePath())
	if err != nil {
		t.Fatal(err)
	}
	job.Put(st, bucketState, "pos", int64(0)) // the crash: the area is stored, the position isn't
	st.Close()
	r2 := fakeRecover(t, dev, int64(len(img)))
	r2.From, r2.Dest, r2.List = r.From, r.Dest, true
	run(t, r2)
	n := 0
	for _, rw := range r2.Rows() {
		if rw.Kind == "unreadable" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d unreadable rows, want the area once", n)
	}
}

// onDisk is a fake card whose physical disk is known.
type onDisk struct{ fakeSrc }

func (o *onDisk) Identity() drives.Identity {
	return drives.Identity{FSSerial: "FAKE0002", PhysicalID: "disk-lock-recover"}
}

// OA3: recover takes the card's disk lock too, as rescue, imaging and the card tools do.
func TestRecoverTakesTheDiskLock(t *testing.T) {
	img := photosCard(t)
	r := fakeRecover(t, bytes.NewReader(img), int64(len(img)))
	r.OpenSource = func(worker.Spec, time.Duration) (Source, error) {
		return &onDisk{fakeSrc{dev: bytes.NewReader(img), size: int64(len(img))}}, nil
	}
	defer r.Close()
	p := plan(t, r)
	unlock, err := job.LockSource(drives.Identity{PhysicalID: "disk-lock-recover"}.DiskLockKey())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := r.Run(context.Background(), env(), p); err == nil {
		t.Fatal("recovered from a disk another job holds")
	}
}
