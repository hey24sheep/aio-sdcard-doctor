package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// rawCard writes an unreadable card (no file system) of size bytes with a photo at off.
func rawCard(t *testing.T, size, off int64, photo []byte) string {
	b := make([]byte, size)
	copy(b[off:], photo)
	p := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Adversarial pass D2: two unreadable cards of the same size shared one identity (size only), so
// the second card's deep scan was skipped and the first card's photo counted as already copied.
func TestTwoRawCardsSameSizeAreDifferentCards(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	a := rawCard(t, 8<<20, 1<<20, realJPEG(t, 120, 90, 3))
	run(t, &Recover{From: a, Dest: dst, Deep: true})
	b := rawCard(t, 8<<20, 3<<20, realJPEG(t, 100, 70, 9))
	r := &Recover{From: b, Dest: dst, Deep: true, StallTimeout: 5e9}
	defer r.Close()
	p, err := r.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range p.Issues {
		if is.Kind == "dest-other-card" {
			return
		}
	}
	t.Fatalf("card B planned into card A's recovery folder: %+v", p.Issues)
}

// Adversarial pass D4: following the hint "run recover again with --select <file>" without
// --deep matched nothing and recovered nothing, clean, exit 0; and a list re-saved by Excel with
// ";" separators matched nothing either.
func TestSelectDeepPicksWithoutDeepFlag(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	photo := realJPEG(t, 120, 90, 5)
	card := rawCard(t, 8<<20, 2<<20, photo)
	run(t, &Recover{From: card, Dest: dst, Deep: true, List: true}) // the list: nothing written
	ids := []string{"C:" + fmt.Sprint(2<<20)}
	_, res := run(t, &Recover{From: card, Dest: dst, Select: ids})
	if res.Counters["CARVED"] != 1 {
		t.Fatalf("picked deep-scan find not recovered: %v", res.Counters)
	}
	r := &Recover{From: card, Dest: filepath.Join(t.TempDir(), "out2"), Select: []string{"C:999999999"}, StallTimeout: 5e9}
	defer r.Close()
	p, err := r.Plan(context.Background(), env())
	if err == nil {
		_, err = r.Run(context.Background(), env(), p)
	}
	if err == nil {
		t.Fatal("picks that match nothing recovered nothing, without a word")
	}
	sel := filepath.Join(t.TempDir(), "picks.csv")
	os.WriteFile(sel, []byte("State;Name;Path;Size;Modified;Comment;ID\nExcellent;a.jpg;a.jpg;1;x;;D:a.jpg\n"), 0o644)
	if got, _ := ReadSelection(sel); len(got) != 1 || got[0] != "D:a.jpg" {
		t.Fatalf("';' list read as %q", got)
	}
}

// Adversarial pass 2 SR2: two deleted files whose assumed runs overlap were both "Excellent",
// though the space can hold only one of them. Two records of the same data aren't a conflict.
func TestDeletedFilesSharingSpaceArePoor(t *testing.T) {
	rows := []Row{
		{Kind: "deleted", Path: "a.jpg", Extents: [][2]int64{{4096, 6000}}, internal: "recoverable"},
		{Kind: "deleted", Path: "b.jpg", Extents: [][2]int64{{8192, 3000}}, internal: "recoverable"},
		{Kind: "deleted", Path: "c.jpg", Extents: [][2]int64{{20480, 100}}, internal: "recoverable"},
		{Kind: "deleted", Path: "d.jpg", Extents: [][2]int64{{20480, 100}}, internal: "recoverable"}, // same data as c
	}
	for i := range rows {
		rows[i].State, rows[i].Comment = mapState(rows[i].internal)
	}
	sharedSpace(rows)
	for i, want := range []string{Poor, Poor, Excellent, Excellent} {
		if rows[i].State != want {
			t.Errorf("%s: %s %q, want %s", rows[i].Path, rows[i].State, rows[i].Comment, want)
		}
	}
	if !strings.Contains(rows[0].Comment, "another deleted file") {
		t.Errorf("comment %q", rows[0].Comment)
	}
}

// twoVolumes is a card with two readable volumes of 4 MiB each.
type twoVolumes struct{ fakeSrc }

func (f *twoVolumes) ReadError() string { return "" }
func (f *twoVolumes) Volumes() []fsread.Volume {
	return []fsread.Volume{{Offset: 0, Size: 4 << 20, FS: "fat32"}, {Offset: 4 << 20, Size: 4 << 20, FS: "fat32"}}
}

// Adversarial pass 2 SR5: recovering --volume 1 into the folder that holds --volume 0's
// recovery counted volume 1's finds as already copied (same keys, same places).
func TestOtherVolumeIntoSameFolderRefused(t *testing.T) {
	img := make([]byte, 8<<20)
	copy(img[1<<20:], realJPEG(t, 300, 200, 5))
	copy(img[5<<20:], realJPEG(t, 300, 200, 6)) // the same place in volume 1
	dst := filepath.Join(t.TempDir(), "out")
	from := filepath.Join(t.TempDir(), "card.img")
	os.WriteFile(from, nil, 0o644)
	open := func(worker.Spec, time.Duration) (Source, error) {
		return &twoVolumes{fakeSrc{dev: bytes.NewReader(img), size: int64(len(img))}}, nil
	}
	run(t, &Recover{From: from, Dest: dst, Deep: true, OpenSource: open})
	r := &Recover{From: from, Dest: dst, Deep: true, Volume: 1, OpenSource: open}
	defer r.Close()
	if p := plan(t, r); !blocked(p, "dest-other-volume") {
		t.Fatalf("issues %+v", p.Issues)
	}
}

// failLater reads fine until fail is set, then fails reads touching [from, to).
type failLater struct {
	r        io.ReaderAt
	fail     *atomic.Bool
	from, to int64
}

func (f failLater) ReadAt(p []byte, off int64) (int, error) {
	if f.fail.Load() && off < f.to && off+int64(len(p)) > f.from {
		return 0, errors.New("I/O error")
	}
	return f.r.ReadAt(p, off)
}

// Adversarial pass 2 M5: a find that can't be read any more when it is recovered was reported
// as a corrupt file ("truncated or corrupt jpeg"), not as unreadable.
func TestUnreadableFindSaysSo(t *testing.T) {
	img := make([]byte, 8<<20)
	jpg := realJPEG(t, 300, 200, 7)
	copy(img[1<<20:], jpg)
	var fail atomic.Bool
	dev := failLater{r: bytes.NewReader(img), fail: &fail, from: 1<<20 + 4096, to: 1<<20 + 8192}
	dst := filepath.Join(t.TempDir(), "out")
	r := &Recover{From: filepath.Join(t.TempDir(), "card.img"), Dest: dst, Deep: true,
		OpenSource: func(worker.Spec, time.Duration) (Source, error) {
			return &fakeSrc{dev: dev, size: int64(len(img))}, nil
		}}
	os.WriteFile(r.From, nil, 0o644)
	defer r.Close()
	p := plan(t, r)
	e := env()
	e.Progress = func(p job.Progress) { fail.Store(p.Phase == "Recovering") } // the card got worse
	if _, err := r.Run(context.Background(), e, p); err != nil {
		t.Fatal(err)
	}
	got := entries(t, dst)["carved@1048576"]
	if got.Status != manifest.CARVED_DAMAGED || !strings.Contains(got.Error, "4096 bytes could not be read") {
		t.Fatalf("entry %+v (all %v)", got, entries(t, dst))
	}
}
