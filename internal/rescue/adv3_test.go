package rescue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/archive"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// meta reads the manifest's metadata of a finished run.
func meta(t *testing.T, dst string) manifest.Manifest {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := manifest.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// The manifest records which folder was copied, so Format can tell a whole-card rescue from a
// rescue of one folder of the card (OA4).
func TestRescueRecordsSourceRoot(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := meta(t, dst).SourceRoot; got != src {
		t.Fatalf("SourceRoot %q, want %q", got, src)
	}
}

// A card file that changed since the last run and now can't be read is not "copied": the old
// copy describes an older version, so the run reports this run's failure and isn't clean (SC1).
func TestChangedFileUnreadableIsNotKeptAsOK(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	if _, err := do(t, memRescue(src, dst, newMemCard(map[string][]byte{rel: bytes.Repeat([]byte("A"), 100000)}, "SC1")), nil, nil); err != nil {
		t.Fatal(err)
	}
	m := newMemCard(map[string][]byte{rel: bytes.Repeat([]byte("B"), 150000)}, "SC1")
	m.openErr[rel] = errors.New("I/O error: CRC")
	res, err := do(t, memRescue(src, dst, m), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := entries(t, dst)[rel]
	if res.Clean || e.Status != manifest.FAILED || res.Counters["OK"] != 0 {
		t.Fatalf("stale copy reported: clean=%v %v row %+v", res.Clean, res.Counters, e)
	}
	if !strings.Contains(e.Error, "older version") || !strings.Contains(e.Error, "IMG_1.JPG") {
		t.Fatalf("row doesn't say an older version is kept: %q", e.Error)
	}
}

// The card pulled out in the middle of a file: what was read is already on the destination as
// __PARTIAL. The row must say so, so a later, worse read never replaces it (OC1).
func TestPulledCardPartialIsRecordedAndKept(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	files := map[string][]byte{rel: bytes.Repeat([]byte("P"), 300000)}
	m := newMemCard(files, "OC1SER")
	m.bad[rel] = []faultreader.Range{{Off: 200000, Len: 100000}} // pulled after 200 KB
	r := memRescue(src, dst, m)
	r.Present = func(drives.Identity, string) (string, bool) { return "", false }
	p := &prompter{answers: map[string]job.Choice{"card-removed": job.Stop}}
	if _, err := do(t, r, p, nil); err == nil {
		t.Fatal("the run went on without the card")
	}
	e := entries(t, dst)[rel]
	if e.Status != manifest.PARTIAL || e.Dest != "Photos/DCIM/IMG_1__PARTIAL.JPG" || e.BadBytes == 0 {
		t.Fatalf("the stored partial copy is not recorded: %+v", e)
	}
	first := e.BadBytes
	m = newMemCard(files, "OC1SER")
	m.bad[rel] = []faultreader.Range{{Off: 0, Len: 300000}} // worse now
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "Photos", "DCIM", "IMG_1__PARTIAL.JPG"))
	if z := bytes.Count(b, []byte{0}); int64(z) != first {
		t.Fatalf("the better partial copy was replaced: %d zero bytes", z)
	}
}

// The card keeps disappearing in the middle of a file: after the last attempt the row points at
// the partial copy on the destination instead of "FAILED, nothing stored" (OC1).
func TestGaveUpKeepsRecordOfStoredPartial(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	m := newMemCard(map[string][]byte{rel: bytes.Repeat([]byte("P"), 300000)}, "OC1BSER")
	m.bad[rel] = []faultreader.Range{{Off: 200000, Len: 100000}}
	r := memRescue(src, dst, m)
	var n atomic.Int32
	r.Present = func(drives.Identity, string) (string, bool) {
		return filepath.Join(src, "again", string(rune('a'+n.Add(1)))), true // back "elsewhere" each time
	}
	res, err := do(t, r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := entries(t, dst)[rel]
	if e.Status != manifest.PARTIAL || e.Dest != "Photos/DCIM/IMG_1__PARTIAL.JPG" || !strings.Contains(e.Error, "gave up") {
		t.Fatalf("row %+v", e)
	}
	if res.Counters["PARTIAL"] != 1 || res.Counters["FAILED"] != 0 || res.Clean {
		t.Fatalf("counters %v clean=%v", res.Counters, res.Clean)
	}
}

// A kept earlier copy's note doesn't say "this run": it is carried into later runs (OC11b).
func TestKeptCopyNoteHasNoThisRun(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	files := map[string][]byte{rel: bytes.Repeat([]byte("P"), 200000)}
	m := newMemCard(files, "OC11B")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	m = newMemCard(files, "OC11B")
	m.openErr[rel] = errors.New("I/O error: CRC")
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	e := entries(t, dst)[rel]
	if e.Status != manifest.PARTIAL || strings.Contains(e.Error, "this run") || !strings.Contains(e.Error, "I/O error") {
		t.Fatalf("row %+v", e)
	}
}

// A read worse than the copy already kept is refused, but the health monitor still hears about
// this read's unreadable bytes, not the old copy's (OC11c).
func TestRefusedWorseReadFeedsMonitor(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/IMG_1.JPG"
	files := map[string][]byte{rel: bytes.Repeat([]byte("P"), 9<<20)}
	m := newMemCard(files, "OC11C")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	m = newMemCard(files, "OC11C")
	m.bad[rel] = []faultreader.Range{{Off: 0, Len: 2 << 20}}
	p := &prompter{}
	if _, err := do(t, memRescue(src, dst, m), p, nil); err != nil {
		t.Fatal(err)
	}
	if e := entries(t, dst)[rel]; e.BadBytes != 4096 {
		t.Fatalf("worse read kept: %+v", e)
	}
	if p.asked["card-degrading"] == 0 {
		t.Fatal("2 MiB unreadable in 9 MiB read raised no question")
	}
}

// lostDir lists the copies of LOST.DIR fragments on the destination.
func lostDir(t *testing.T, dst string) []string {
	t.Helper()
	var names []string
	filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(p, "LOST.DIR") {
			names = append(names, p[len(dst):])
		}
		return nil
	})
	return names
}

// A LOST.DIR fragment is planned under its sniffed extension; its earlier copy must be claimed
// under that same name, or Start over moves it to a ~hash name beside the old one (OC2, SC2).
func TestFragmentKeepsItsNameOnStartOver(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	files := map[string][]byte{"LOST.DIR/1234": testutil.JPEG(30000, "frag"), "DCIM/a.jpg": testutil.JPEG(20000, "a")}
	if _, err := do(t, memRescue(src, dst, newMemCard(files, "OC2SER")), nil, nil); err != nil {
		t.Fatal(err)
	}
	first := entries(t, dst)["LOST.DIR/1234"]
	r := memRescue(src, dst, newMemCard(files, "OC2SER"))
	r.Fresh = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	second := entries(t, dst)["LOST.DIR/1234"]
	if names := lostDir(t, dst); first.Dest != second.Dest || len(names) != 1 {
		t.Fatalf("Start over moved the fragment's copy (%s → %s); on disk: %v", first.Dest, second.Dest, names)
	}
}

// A partial fragment read fully on the next run replaces its __PARTIAL copy (OC2).
func TestRepairedFragmentLeavesNoPartial(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "LOST.DIR/1234"
	files := map[string][]byte{rel: testutil.JPEG(30000, "frag")}
	m := newMemCard(files, "OC2BSER")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, memRescue(src, dst, newMemCard(files, "OC2BSER")), nil, nil); err != nil {
		t.Fatal(err)
	}
	if names := lostDir(t, dst); len(names) != 1 || entries(t, dst)[rel].Status != manifest.OK {
		t.Fatalf("row %+v, on disk %v", entries(t, dst)[rel], names)
	}
}

// An earlier run's a__PARTIAL.jpg is never overwritten by a card file literally named
// a__PARTIAL.jpg, even when a.jpg isn't on the card this time (SC3).
func TestPartialCopyNotOverwrittenByLookalikeName(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	big := bytes.Repeat([]byte("P"), 100000)
	m := newMemCard(map[string][]byte{"DCIM/a.jpg": big}, "SC3SER")
	m.bad["DCIM/a.jpg"] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dst, "Photos", "DCIM", "a__PARTIAL.jpg")
	before, _ := os.ReadFile(p)
	other := bytes.Repeat([]byte("Q"), 50000)
	if _, err := do(t, memRescue(src, dst, newMemCard(map[string][]byte{"DCIM/a__PARTIAL.jpg": other}, "SC3SER")), nil, nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("the earlier partial copy was overwritten")
	}
	e := entries(t, dst)["DCIM/a__PARTIAL.jpg"]
	if e.Status != manifest.OK || e.Dest == "Photos/DCIM/a__PARTIAL.jpg" {
		t.Fatalf("lookalike %+v", e)
	}
}

// syncFailOnce fails the first fsync of a ZIP part with "disk full".
type syncFailOnce struct {
	destfs.FS
	done *atomic.Bool
}

type sfFile struct {
	destfs.File
	fail bool
}

func (f sfFile) Sync() error {
	if f.fail {
		return &os.PathError{Op: "sync", Path: "part", Err: diskFullErrno}
	}
	return f.File.Sync()
}

func (s syncFailOnce) Create(p string) (destfs.File, error) {
	f, err := s.FS.Create(p)
	if err != nil {
		return f, err
	}
	return sfFile{f, strings.HasSuffix(p, ".zip.tmp") && s.done.CompareAndSwap(false, true)}, nil
}

// OC3: a ZIP part hit "disk full" as it was closed; the user freed space and chose Retry. The
// file put again must not be reported FAILED for the discarded part.
func TestZipCloseDiskFullRetryKeepsGoodCopy(t *testing.T) {
	old := archive.MaxPartBytes
	archive.MaxPartBytes = 1 // every file closes its part
	defer func() { archive.MaxPartBytes = old }()
	card := testutil.Card{"DCIM/a.jpg": {Data: testutil.Random(100000, 1)}, "DCIM/b.jpg": {Data: testutil.Random(100001, 2)}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	os.MkdirAll(dst, 0o755)
	p := &prompter{answers: map[string]job.Choice{"disk-full": job.Retry}}
	res, err := do(t, r, p, syncFailOnce{destfs.New(dst, src), new(atomic.Bool)})
	if err != nil {
		t.Fatal(err)
	}
	for k, e := range entries(t, dst) {
		if e.Status != manifest.OK {
			t.Errorf("%s %s %q", k, e.Status, e.Error)
		}
	}
	if !res.Clean || res.Counters["OK"] != 2 {
		t.Fatalf("counters %v clean=%v", res.Counters, res.Clean)
	}
}

// SC4: a crashed run then a run in the other mode (Compress toggled) left the first mode's temp
// files forever. Every run cleans both.
func TestRunCleansOtherModesTemps(t *testing.T) {
	for _, compress := range []bool{false, true} {
		src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
		r := newRescue(src, dst, newFake(t, src))
		r.Compress = !compress
		if _, err := do(t, r, nil, nil); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(dst, archive.TmpDir), 0o755)
		os.WriteFile(filepath.Join(dst, archive.TmpDir, strings.Repeat("ab", 20)+".tmp"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(dst, "Photos-0001.zip.tmp"), []byte("x"), 0o644)
		r = newRescue(src, dst, newFake(t, src))
		r.Compress = compress
		if _, err := do(t, r, nil, nil); err != nil {
			t.Fatal(err)
		}
		noTmp(t, dst)
	}
}

// OC10: ZIP parts are opened in Explorer too, so their entry names follow Windows' rules whatever
// file system the parts are written to.
func TestZipEntryNamesUseWindowsRules(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	r := memRescue(src, dst, newMemCard(map[string][]byte{"DCIM/a:b?.jpg": testutil.JPEG(3000, "a")}, "OC10SER"))
	r.LookupDrive = func(p string) (drives.Drive, error) {
		if p == src {
			return drives.Drive{Root: src, Removable: true, PhysicalID: "card:" + src, FS: "ext4"}, nil
		}
		return drives.Drive{Root: "/", PhysicalID: "disk", FS: "ext4", Free: 1 << 40}, nil
	}
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d := entries(t, dst)["DCIM/a:b?.jpg"].Dest; d != "Photos-0001.zip/DCIM/a_b_.jpg" {
		t.Fatalf("stored as %q", d)
	}
}

// SU1: a pasted destination with its quotes made the write guard panic (the window closed, or
// the CLI printed a Go stack). Plan says it as a plain error.
func TestPlanRefusesQuotedDestination(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.CameraCard())
	dst := `"` + filepath.Join(t.TempDir(), "dq") + `"`
	r := newRescue(src, dst, newFake(t, src))
	defer r.Close()
	_, err := r.Plan(context.Background(), job.Env{})
	if err == nil || !strings.Contains(err.Error(), "quotes") {
		t.Fatalf("err %v", err)
	}
}

// OC9: Start over in ZIP mode adds every file again (the old parts stay), so the space check
// counts every file, not only the ones not yet copied.
func TestStartOverCompressCountsEveryFile(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	r = newRescue(src, dst, newFake(t, src))
	r.Compress, r.Fresh = true, true
	var total int64
	r.Free = func(string) (uint64, error) { return uint64(1<<30 + total/2), nil } // room for half the card
	for _, f := range testutil.CameraCard() {
		total += int64(len(f.Data))
	}
	p, err := r.Plan(context.Background(), job.Env{})
	defer r.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range p.Issues {
		if is.Kind == "short-space" {
			return
		}
	}
	t.Fatalf("no short-space warning: %+v", p.Issues)
}

// OA3: a card is also locked by its physical disk, which every job reading that disk takes, so a
// card without a serial is locked the same way whichever identity each job worked out for it.
func TestRescueTakesTheDiskLock(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	key := drives.Identity{PhysicalID: "card:" + src}.DiskLockKey()
	unlock, err := job.LockSource(key)
	if err != nil {
		t.Fatal(err)
	}
	r := newRescue(src, dst, newFake(t, src))
	p, err := r.Plan(context.Background(), job.Env{})
	r.Close()
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !blockedBy(p, "busy") {
		t.Fatalf("ran while another job held the card's disk: %+v", p.Issues)
	}
	// and while a rescue holds it, another job can't take it
	r = newRescue(src, dst, newFake(t, src))
	if _, err := r.Plan(context.Background(), job.Env{}); err != nil {
		t.Fatal(err)
	}
	_, err = job.LockSource(key)
	r.Close()
	if !errors.Is(err, job.ErrSourceBusy) {
		t.Fatalf("disk lock not held by the rescue: %v", err)
	}
}

func blockedBy(p job.Plan, kind string) bool {
	for _, is := range p.Issues {
		if is.Kind == kind && is.Severity == job.Block {
			return true
		}
	}
	return false
}

// OC5: the same card read as a mounted folder and then raw (or as an image) can show every time
// shifted by the local UTC offset (exFAT times without an offset, Linux vfat's sys_tz). A whole
// number of quarter hours up to ±14 h, seconds within FAT's 2 s, is the same time.
func TestSameTimeAcrossTimeZones(t *testing.T) {
	a := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{5*time.Hour + 30*time.Minute, -(9*time.Hour + 45*time.Minute), 14 * time.Hour, -14 * time.Hour,
		5*time.Hour + 45*time.Minute + 2*time.Second, 3*time.Hour - time.Second, time.Second} {
		if !sameTime(a, a.Add(d)) {
			t.Errorf("%v apart: not the same time", d)
		}
	}
	for _, d := range []time.Duration{14*time.Hour + 15*time.Minute, 5*time.Hour + 31*time.Minute, 7 * time.Minute, 3 * time.Second, 24 * time.Hour} {
		if sameTime(a, a.Add(d)) {
			t.Errorf("%v apart: taken for the same time", d)
		}
	}
}

// destGone: the destination disk disappears (USB unplugged, share dropped) after the first file.
type destGone struct {
	destfs.FS
	n *atomic.Int32
}

func (d destGone) Create(p string) (destfs.File, error) {
	if strings.Contains(p, archive.TmpDir) && d.n.Add(1) > 1 {
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
	return d.FS.Create(p)
}

// OC6: a destination that went away was blamed on the card ("file disappeared from the card",
// "the card is degrading"). The run stops and says the destination can't be reached.
func TestDestinationGoneIsNotBlamedOnCard(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	os.MkdirAll(dst, 0o755)
	p := &prompter{}
	_, err := do(t, newRescue(src, dst, newFake(t, src)), p, destGone{destfs.New(dst, src), new(atomic.Int32)})
	if err == nil || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("err %v", err)
	}
	if p.asked["card-degrading"]+p.asked["card-removed"] > 0 {
		t.Fatalf("blamed on the card: %v", p.asked)
	}
	for k, e := range entries(t, dst) {
		if strings.Contains(e.Error, "card") {
			t.Errorf("%s: %q", k, e.Error)
		}
	}
}

// OC7: resume trusted the file-system serial alone; devices that give every card one serial
// resumed card B into card A's folder. A different size (read the same way), or none of a sample
// of copied files still on the card, means a different card.
func TestResumeRefusesOtherCardWithSameSerial(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i < 8; i++ {
		files["DCIM/a"+string(rune('0'+i))+".jpg"] = testutil.JPEG(2000+i, "a")
	}
	other := map[string][]byte{"DCIM/b1.jpg": testutil.JPEG(3000, "b"), "DCIM/b2.jpg": testutil.JPEG(3001, "b")}
	plan := func(m *memCard, src, dst string) job.Plan {
		t.Helper()
		r := memRescue(src, dst, m)
		defer r.Close()
		p, err := r.Plan(context.Background(), job.Env{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	if _, err := do(t, memRescue(src, dst, newMemCard(files, "FIXEDSER")), nil, nil); err != nil {
		t.Fatal(err)
	}
	if p := plan(newMemCard(other, "FIXEDSER"), src, dst); !blockedBy(p, "dest-other-card") {
		t.Fatalf("other card's files, same serial: %+v", p.Issues)
	}
	b := newMemCard(files, "FIXEDSER")
	b.id.Size = 32 << 30
	if p := plan(b, src, dst); !blockedBy(p, "dest-other-card") {
		t.Fatalf("other size, same serial: %+v", p.Issues)
	}
	more := map[string][]byte{"DCIM/new.jpg": testutil.JPEG(4000, "n")}
	for k, v := range files {
		more[k] = v
	}
	if p := plan(newMemCard(more, "FIXEDSER"), src, dst); blockedBy(p, "dest-other-card") {
		t.Fatalf("the same card refused: %+v", p.Issues)
	}
}

// summaryText is the newest copy summary's text.
func summaryText(t *testing.T, dst string) string {
	t.Helper()
	sums, _ := filepath.Glob(filepath.Join(dst, "Reports", "copy_summary_*.txt"))
	if len(sums) == 0 {
		t.Fatal("no summary")
	}
	sort.Strings(sums)
	b, _ := os.ReadFile(sums[len(sums)-1])
	return string(b)
}

// failingPhotos is a card whose photos can't be read (the card degrades) and one video.
func failingPhotos(serial string) *memCard {
	files := map[string][]byte{"DCIM/v1.mp4": bytes.Repeat([]byte("v"), 50000)}
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("DCIM/p%d.jpg", i)] = bytes.Repeat([]byte{byte('0' + i)}, 1000+i)
	}
	m := newMemCard(files, serial)
	for k := range files {
		if strings.HasSuffix(k, ".jpg") {
			m.openErr[k] = errors.New("I/O error: CRC")
		}
	}
	return m
}

// SC5: the user chose to skip the remaining videos: the summary says how many were left out.
func TestSkippedVideosCountedInSummary(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	p := &prompter{answers: map[string]job.Choice{"card-degrading": SkipVideos}}
	if _, err := do(t, memRescue(src, dst, failingPhotos("SC5SER")), p, nil); err != nil {
		t.Fatal(err)
	}
	if s := summaryText(t, dst); !strings.Contains(s, "1 video left out because you chose to skip") {
		t.Fatalf("summary:\n%s", s)
	}
}

// titles records every question's title.
type titles struct {
	prompter
	seen []string
}

func (p *titles) Ask(_ context.Context, is job.Issue) job.Choice {
	p.seen = append(p.seen, is.Title)
	return p.ask(is)
}

// SC6b: on a resume, "the card is degrading — N files left" counted only files with no row yet,
// so it said 0 when every file had failed before.
func TestDegradingCountsFilesLeftOnResume(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	cont := map[string]job.Choice{"card-degrading": job.Continue}
	if _, err := do(t, memRescue(src, dst, failingPhotos("SC6BSER")), &prompter{answers: cont}, nil); err != nil {
		t.Fatal(err)
	}
	p := &titles{prompter: prompter{answers: cont}}
	r := memRescue(src, dst, failingPhotos("SC6BSER"))
	env := job.Env{Prompt: p}
	plan, err := r.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Run(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	if len(p.seen) == 0 || !strings.Contains(p.seen[0], "3 files") {
		t.Fatalf("questions %q (6 photos to read, 3 failed so far; the video is copied)", p.seen)
	}
}

// SC6c: the resume banner counted rows of a ZIP part that was never finished as done.
func TestResumeBannerCountsOnlyStoredZipEntries(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.Card{"DCIM/a.jpg": {Data: testutil.JPEG(2000, "a")},
		"DCIM/b.jpg": {Data: testutil.JPEG(2001, "b")}, "DCIM/c.jpg": {Data: testutil.JPEG(2002, "c")}}), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Compress, r.Types = true, nil
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.Open(s)
	for _, rel := range []string{"DCIM/a.jpg", "DCIM/b.jpg"} { // as if the run crashed before their part was closed
		e, _, _ := m.Get(rel)
		e.Dest = "Photos-0009.zip/" + rel
		m.Put(e)
	}
	s.Close()
	r = newRescue(src, dst, newFake(t, src))
	r.Compress, r.Types = true, nil
	p, err := r.Plan(context.Background(), job.Env{})
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range p.Issues {
		if is.Kind == "resume" {
			if !strings.Contains(is.Title, "1 of 3") {
				t.Fatalf("banner %q", is.Title)
			}
			return
		}
	}
	t.Fatalf("no resume banner: %+v", p.Issues)
}

// SC6d: a file whose ZIP part failed is FAILED: its header-check line leaves the summary too.
func TestFailedZipPartDropsHeaderLines(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	r := memRescue(src, dst, newMemCard(map[string][]byte{"DCIM/x.jpg": testutil.PNG(3000, "x")}, "SC6DSER"))
	r.Compress = true
	os.MkdirAll(dst, 0o755)
	do(t, r, nil, renameFails{destfs.New(dst, src)})
	if e := entries(t, dst)["DCIM/x.jpg"]; e.Status != manifest.FAILED {
		t.Fatalf("row %+v", e)
	}
	if s := summaryText(t, dst); strings.Contains(s, "] DCIM/x.jpg") {
		t.Fatalf("summary:\n%s", s)
	}
}

// OC11a: files outside this choice kept from an earlier run are not "excluded because you chose
// to skip them"; the summary shows how many were kept.
func TestSummaryKeptNotCountedAsExcluded(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Types = nil
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	r = newRescue(src, dst, newFake(t, src))
	r.Types = []profile.Category{profile.Photos}
	res, err := do(t, r, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	kept := res.Counters["KEPT-FROM-EARLIER-RUN"]
	if kept == 0 {
		t.Fatal("nothing kept: the test needs non-photos on the card")
	}
	s := summaryText(t, dst)
	if strings.Contains(s, "type not selected") || !strings.Contains(s, fmt.Sprintf("Kept from an earlier run (outside this choice, left as they are): %d", kept)) {
		t.Fatalf("summary:\n%s", s)
	}
}

// OC11d: a "same name twice in a damaged folder" row stayed after the folder read fine again.
func TestDuplicateNameRowGoesWhenFolderHealthy(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	files := map[string][]byte{"DCIM/a.jpg": testutil.JPEG(2000, "a")}
	m := newMemCard(files, "OC11DSER")
	m.dup = map[string]bool{"DCIM": true}
	do(t, memRescue(src, dst, m), nil, nil)
	n := 0
	for k := range entries(t, dst) {
		if strings.Contains(k, "with this name") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("damaged run: %d duplicate rows", n)
	}
	res, err := do(t, memRescue(src, dst, newMemCard(files, "OC11DSER")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, e := range entries(t, dst) {
		if strings.Contains(k, "with this name") {
			t.Fatalf("stale row %s %+v", k, e)
		}
	}
	if !res.Clean {
		t.Fatalf("healthy run not clean: %v", res.Counters)
	}
}

// OC11e: a folder __PARTIAL copy superseded by a better entry in a (finished) ZIP part is
// removed: it is our own file, and the manifest no longer points at it.
func TestZipEntrySupersedesFolderPartial(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	rel := "DCIM/a.jpg"
	files := map[string][]byte{rel: testutil.JPEG(30000, "a")}
	m := newMemCard(files, "OC11ESER")
	m.bad[rel] = []faultreader.Range{{Off: 8192, Len: 4096}}
	if _, err := do(t, memRescue(src, dst, m), nil, nil); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dst, "Photos", "DCIM", "a__PARTIAL.jpg")
	if _, err := os.Stat(partial); err != nil {
		t.Fatal(err)
	}
	r := memRescue(src, dst, newMemCard(files, "OC11ESER"))
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	if e := entries(t, dst)[rel]; e.Status != manifest.OK || !strings.HasPrefix(e.Dest, "Photos-0001.zip/") {
		t.Fatalf("row %+v", e)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Fatal("stale partial left beside the ZIP entry")
	}
}
