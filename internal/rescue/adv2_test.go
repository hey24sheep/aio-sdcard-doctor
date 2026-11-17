package rescue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// memCard is an in-memory card with case-sensitive names (like ext4, or a damaged FAT folder),
// which a folder on Windows or macOS can't hold.
type memCard struct {
	fstest.MapFS
	id      drives.Identity
	bad     map[string][]faultreader.Range
	openErr map[string]error
	opens   map[string]int
	dup     map[string]bool // folders listing their first name twice
}

var memMTime = time.Date(2024, 1, 2, 10, 0, 0, 0, time.Local)

func newMemCard(files map[string][]byte, serial string) *memCard {
	m := &memCard{MapFS: fstest.MapFS{}, id: drives.Identity{FSSerial: serial, Size: 64 << 30},
		bad: map[string][]faultreader.Range{}, openErr: map[string]error{}, opens: map[string]int{}}
	for k, v := range files {
		m.MapFS[k] = &fstest.MapFile{Data: v, ModTime: memMTime, Mode: 0o644}
	}
	return m
}

func (m *memCard) OpenAt(name string) (io.ReaderAt, int64, io.Closer, error) {
	m.opens[name]++
	if err := m.openErr[name]; err != nil {
		return nil, 0, nil, err
	}
	f, ok := m.MapFS[name]
	if !ok {
		return nil, 0, nil, fs.ErrNotExist
	}
	return faultreader.New(f.Data, m.bad[name], faultreader.Options{}), int64(len(f.Data)), io.NopCloser(nil), nil
}

// ReadDir lists a folder, with its first name twice when dup says so (a damaged folder).
func (m *memCard) ReadDir(name string) ([]fs.DirEntry, error) {
	es, err := m.MapFS.ReadDir(name)
	if m.dup[name] && len(es) > 0 {
		es = append(es, es[0])
	}
	return es, err
}

func (m *memCard) Kind() string                       { return "ext4" }
func (m *memCard) Identity() drives.Identity          { return m.id }
func (m *memCard) Retarget(worker.Spec) error         { return nil }
func (m *memCard) ResetBudget()                       {}
func (m *memCard) Close() error                       { return nil }
func (m *memCard) Stat(n string) (fs.FileInfo, error) { return fs.Stat(m.MapFS, n) }

func memRescue(src, dst string, m *memCard) *Rescue {
	r := newRescue(src, dst, nil)
	r.OpenSource = func(worker.Spec, time.Duration) (Source, error) { return m, nil }
	return r
}

// renameFails refuses to rename finished ZIP parts into place (a destination that went away or
// an antivirus holding the file).
type renameFails struct{ destfs.FS }

func (f renameFails) Rename(a, b string) error {
	if strings.HasSuffix(a, ".zip.tmp") {
		return errors.New("simulated rename failure")
	}
	return f.FS.Rename(a, b)
}

// C1: a verified ZIP part that couldn't be renamed into place left its files reported OK and
// the run clean, while the next run deletes the leftover .zip.tmp.
func TestZipPartNotRenamedIsReportedFailed(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	res, _ := do(t, r, nil, renameFails{destfs.New(dst, src)})
	if res.Clean || res.Counters["OK"] != 0 {
		t.Fatalf("no ZIP part reached the destination, yet: %v clean=%v", res.Counters, res.Clean)
	}
	for k, e := range entries(t, dst) {
		if e.Status == manifest.OK {
			t.Errorf("%s reported OK at %s, but its part was never renamed into place", k, e.Dest)
		}
	}
	// the next run copies them again
	r = newRescue(src, dst, newFake(t, src))
	r.Compress = true
	if res, err := do(t, r, nil, nil); err != nil || !res.Clean || res.Counters["OK"] != len(card) {
		t.Fatalf("second run: %v clean=%v err=%v", res.Counters, res.Clean, err)
	}
}

// C2: a run that can't read a file at all used to overwrite its better PARTIAL row (Dest and bad
// bytes lost); the run after that then let a worse read replace the better copy on disk.
func TestFailedRereadKeepsBetterPartial(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	files := map[string][]byte{rel: bytes.Repeat([]byte("P"), 200000)}
	m := newMemCard(files, "C2SERIAL")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	first := entries(t, dst)[rel]
	if first.Status != manifest.PARTIAL || first.Dest == "" {
		t.Fatalf("first run: %+v", first)
	}
	m = newMemCard(files, "C2SERIAL")
	m.openErr[rel] = errors.New("I/O error: CRC")
	res, err := do(t, memRescue(src, dst, m), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := entries(t, dst)[rel]
	if e.Status != manifest.PARTIAL || e.Dest != first.Dest || e.BadBytes != first.BadBytes || e.SHA256 != first.SHA256 {
		t.Fatalf("a failed re-read replaced the better row: %+v", e)
	}
	if !strings.Contains(e.Error, "I/O error") {
		t.Errorf("this run's failure isn't noted: %q", e.Error)
	}
	if res.Counters["PARTIAL"] != 1 || res.Counters["FAILED"] != 0 || res.Clean {
		t.Errorf("counters %v clean=%v: the destination holds a partial copy", res.Counters, res.Clean)
	}
	m = newMemCard(files, "C2SERIAL")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 65536}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(first.Dest)))
	if n := bytes.Count(b, []byte{0}); n != 4096 {
		t.Fatalf("the 4 KiB-bad copy was replaced by one with %d zero bytes", n)
	}
}

// C3: a case-clashing pair split across runs gave the newly listed member the plain name, which
// overwrote the copy an earlier run recorded for the other one on a case-insensitive destination.
func TestCaseClashAcrossRunsKeepsEarlierCopy(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	files := map[string][]byte{"notes/a.txt": []byte("AAAA"), "notes/A.TXT": []byte("BBBB")}
	m := newMemCard(files, "C3SERIAL")
	delete(m.MapFS, "notes/A.TXT") // not listed in the first run (a damaged entry)
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	first := entries(t, dst)["notes/a.txt"]
	for i := 0; i < 2; i++ {
		if _, err := do(t, memRescue(src, dst, newMemCard(files, "C3SERIAL")), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	es := entries(t, dst)
	if es["notes/a.txt"].Dest != first.Dest {
		t.Errorf("a.txt moved from %s to %s", first.Dest, es["notes/a.txt"].Dest)
	}
	for rel, want := range files {
		e := es[rel]
		if e.Status != manifest.OK && e.Status != manifest.ALREADY_COPIED {
			t.Errorf("%s: %+v", rel, e)
			continue
		}
		if b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.Dest))); !bytes.Equal(b, want) {
			t.Errorf("%s's copy %s holds %q", rel, e.Dest, b)
		}
	}
	if strings.EqualFold(es["notes/a.txt"].Dest, es["notes/A.TXT"].Dest) {
		t.Errorf("both files share one destination name: %s", es["notes/a.txt"].Dest)
	}
}

// I1: Start over let any new copy replace the recorded one, even a worse one.
func TestStartOverNeverReplacesBetterWithWorse(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	data := bytes.Repeat([]byte("P"), 200000)
	files := map[string][]byte{"DCIM/IMG_1.JPG": data, "DCIM/IMG_2.JPG": data}
	m := newMemCard(files, "I1SERIAL")
	m.bad["DCIM/IMG_1.JPG"] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	m = newMemCard(files, "I1SERIAL")
	m.bad["DCIM/IMG_1.JPG"] = []faultreader.Range{{Off: 8192, Len: 65536}}
	m.bad["DCIM/IMG_2.JPG"] = []faultreader.Range{{Off: 0, Len: 200000}}
	r := memRescue(src, dst, m)
	r.Fresh = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	es := entries(t, dst)
	b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(es["DCIM/IMG_1.JPG"].Dest)))
	if n := bytes.Count(b, []byte{0}); n != 4096 || es["DCIM/IMG_1.JPG"].BadBytes != 4096 {
		t.Errorf("Start over replaced the 4 KiB-bad copy: %d zero bytes, row %+v", n, es["DCIM/IMG_1.JPG"])
	}
	if e := es["DCIM/IMG_2.JPG"]; e.Status != manifest.OK {
		t.Errorf("Start over replaced a good copy with an unreadable one: %+v", e)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Photos", "DCIM", "IMG_2.JPG")); !bytes.Equal(b, data) {
		t.Error("the good copy of IMG_2 was changed")
	}
}

// I2: a deleted __PARTIAL copy was never written again (only deleted OK copies were).
func TestDeletedPartialIsCopiedAgain(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	files := map[string][]byte{rel: bytes.Repeat([]byte("P"), 200000)}
	m := newMemCard(files, "I2SERIAL")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dst, filepath.FromSlash(entries(t, dst)[rel].Dest))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	m = newMemCard(files, "I2SERIAL")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 65536}} // worse than the deleted copy
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	e := entries(t, dst)[rel]
	if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(e.Dest))); err != nil || e.BadBytes != 65536 {
		t.Fatalf("no copy written again: %+v (%v)", e, err)
	}
}

// I3: "skip the remaining videos" turned videos copied by an earlier run into EXCLUDED rows.
func TestSkipVideosKeepsEarlierCopies(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	files := map[string][]byte{
		"DCIM/p1.jpg": bytes.Repeat([]byte("1"), 1000), "DCIM/p2.jpg": bytes.Repeat([]byte("2"), 1001),
		"DCIM/p3.jpg": bytes.Repeat([]byte("3"), 1002), "DCIM/v1.mp4": bytes.Repeat([]byte("v"), 50000),
	}
	failing := func() *memCard {
		m := newMemCard(files, "I3SERIAL")
		for _, p := range []string{"DCIM/p1.jpg", "DCIM/p2.jpg", "DCIM/p3.jpg"} {
			m.openErr[p] = errors.New("I/O error: CRC")
		}
		return m
	}
	if _, err := do(t, memRescue(src, dst, failing()), &prompter{answers: map[string]job.Choice{"card-degrading": job.Continue}}, nil); err != nil {
		t.Fatal(err)
	}
	first := entries(t, dst)["DCIM/v1.mp4"]
	if first.Status != manifest.OK {
		t.Fatalf("first run: %+v", first)
	}
	p := &prompter{answers: map[string]job.Choice{"card-degrading": SkipVideos}}
	res, err := do(t, memRescue(src, dst, failing()), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.asked["card-degrading"] == 0 {
		t.Fatal("the question was never asked")
	}
	if e := entries(t, dst)["DCIM/v1.mp4"]; e.Status != manifest.OK || e.Dest != first.Dest {
		t.Fatalf("a copied video was overwritten by the skip: %+v (counters %v)", e, res.Counters)
	}
}

// I4: rows turned FAILED after the run (a ZIP part not finished) left the summary saying OK.
func TestSummaryAgreesWithCSVAfterZipPartFails(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	do(t, r, nil, renameFails{destfs.New(dst, src)})
	sums, _ := filepath.Glob(filepath.Join(dst, "Reports", "copy_summary_*.txt"))
	if len(sums) != 1 {
		t.Fatalf("summaries: %v", sums)
	}
	b, _ := os.ReadFile(sums[0])
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "OK" {
			t.Errorf("summary still counts OK files: %q", l)
		}
	}
}

// I5: in ZIP mode a damaged state.db rebuilt nothing (a ZIP destination was hashed as a plain
// path and the ZIP index was gone), so every file was read off the card again.
func TestZipRescueRebuiltNotReread(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dst, "Reports", "state.db"), make([]byte, 64<<10), 0o644)
	r = newRescue(src, dst, newFake(t, src))
	r.Compress = true
	var logs []string
	env := job.Env{Prompt: &prompter{}, Log: func(_, msg string) { logs = append(logs, msg) }}
	plan, err := r.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, err := r.Run(context.Background(), env, plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["ALREADY-COPIED"] != len(card) {
		t.Fatalf("not rebuilt from the ZIP parts: %v", res.Counters)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Rebuilt progress") {
		t.Errorf("the rebuild wasn't logged: %q", logs)
	}
}

// I6: a card file literally named foo__PARTIAL.jpg next to foo.jpg: foo.jpg's partial copy
// overwrote it, and a later clean read of foo.jpg deleted it, while the report said OK.
func TestPartialNameNeverHitsAnotherCardFile(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	good := bytes.Repeat([]byte("G"), 9000)
	files := map[string][]byte{"DCIM/foo__PARTIAL.jpg": good, "DCIM/foo.jpg": bytes.Repeat([]byte("F"), 20000)}
	m := newMemCard(files, "I6SERIAL")
	m.bad["DCIM/foo.jpg"] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	check := func(run string) {
		t.Helper()
		e := entries(t, dst)["DCIM/foo__PARTIAL.jpg"]
		if b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.Dest))); !bytes.Equal(b, good) {
			t.Fatalf("%s: the copy of foo__PARTIAL.jpg (%s %s) was overwritten or deleted", run, e.Status, e.Dest)
		}
	}
	check("run 1")
	if _, err := do(t, memRescue(src, dst, newMemCard(files, "I6SERIAL")), nil, nil); err != nil {
		t.Fatal(err)
	}
	check("run 2")
}

// reportsFail can't write the CSV report (destination full or gone at the very end).
type reportsFail struct{ destfs.FS }

func (f reportsFail) Create(p string) (destfs.File, error) {
	if strings.HasSuffix(p, ".csv.tmp") {
		return nil, errors.New("simulated: no space left")
	}
	return f.FS.Create(p)
}

// I7: report write errors were dropped; the run pointed at a CSV that didn't exist.
func TestReportWriteFailureIsTold(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	var logs []string
	env := job.Env{Prompt: &prompter{}, Out: reportsFail{destfs.New(dst, src)}, Log: func(_, msg string) { logs = append(logs, msg) }}
	plan, err := r.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, _ := r.Run(context.Background(), env, plan)
	for _, p := range res.Reports {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the run points at a report that isn't there: %s", p)
		}
	}
	if res.Clean {
		t.Error("run reported clean without its report")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "no space left") {
		t.Errorf("the failure wasn't told: %q", logs)
	}
}

// M1: in compress mode a file is spooled and then added to its part: it needs about twice its
// size free, not once.
func TestCompressSpaceCheckCountsTheSpool(t *testing.T) {
	card := testutil.Card{"DCIM/a.jpg": {Data: testutil.JPEG(60_000, "a")}}
	for _, compress := range []bool{false, true} {
		src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
		r := newRescue(src, dst, newFake(t, src))
		r.Compress = compress
		r.Free = func(string) (uint64, error) { return preflight.Reserve + 100_000, nil }
		p := &prompter{answers: map[string]job.Choice{"short-space": job.Continue}}
		do(t, r, p, nil)
		if asked := p.asked["disk-full"] > 0; asked != compress {
			t.Errorf("compress=%v: disk-full asked=%v", compress, asked)
		}
	}
}

// M4: a file given up on after repeated "destination full → retry" was blamed on the card.
func TestGivingUpOnAFullDestinationSaysSo(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	os.MkdirAll(dst, 0o755)
	full := &os.PathError{Op: "write", Path: "x", Err: diskFullErrno}
	p := &prompter{answers: map[string]job.Choice{"disk-full": job.Retry}}
	do(t, newRescue(src, dst, newFake(t, src)), p, destfs.NewQuota(destfs.New(dst, src), 30_000, full))
	n := 0
	for rel, e := range entries(t, dst) {
		if e.Status == manifest.FAILED && strings.Contains(e.Error, "gave up") {
			n++
			if strings.Contains(e.Error, "card") || !strings.Contains(e.Error, "full") {
				t.Errorf("%s: %q", rel, e.Error)
			}
		}
	}
	if n == 0 {
		t.Fatal("no file was given up on")
	}
}
