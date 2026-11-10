package rescue

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// do plans and runs r with prompter p.
func do(t *testing.T, r *Rescue, p *prompter, out destfs.FS) (job.Result, error) {
	t.Helper()
	if p == nil {
		p = &prompter{}
	}
	env := job.Env{Prompt: p, Out: out}
	plan, err := r.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return r.Run(context.Background(), env, plan)
}

func entries(t *testing.T, dst string) map[string]manifest.Entry {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := manifest.Open(s)
	out := map[string]manifest.Entry{}
	m.Each(func(e manifest.Entry) error { out[e.Source] = e; return nil })
	return out
}

func checkCopies(t *testing.T, card testutil.Card, dst string, es map[string]manifest.Entry) {
	t.Helper()
	for rel, f := range card {
		e, ok := es[rel]
		if !ok {
			t.Errorf("%s: no manifest entry", rel)
			continue
		}
		if e.Status != manifest.OK {
			t.Errorf("%s: %s %s", rel, e.Status, e.Error)
			continue
		}
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.Dest)))
		if err != nil || !bytes.Equal(b, f.Data) {
			t.Errorf("%s → %s: copy differs (%v)", rel, e.Dest, err)
		}
	}
}

func noTmp(t *testing.T, dst string) {
	t.Helper()
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".tmp") {
			t.Errorf("tmp left: %s", p)
		}
		return nil
	})
}

// TestRescueFixtureByteIdentical goes through the real worker process.
func TestRescueFixtureByteIdentical(t *testing.T) {
	for name, card := range map[string]testutil.Card{"android": testutil.AndroidCard(false), "camera": testutil.CameraCard(),
		"gopro": testutil.GoProCard(), "dji": testutil.DJICard(), "avchd": testutil.AVCHDCard()} {
		t.Run(name, func(t *testing.T) {
			src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
			r := &Rescue{From: src, Dest: dst, StallTimeout: 10 * time.Second, LookupDrive: testDrives(src, dst)}
			res, err := do(t, r, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			es := entries(t, dst)
			checkCopies(t, card, dst, es)
			if res.Counters["OK"] != len(card) || !res.Clean {
				t.Fatalf("counters %v", res.Counters)
			}
			for rel, f := range card {
				if f.Header != "" && es[rel].Header != f.Header {
					t.Errorf("%s header %q want %q", rel, es[rel].Header, f.Header)
				}
				if st, err := os.Stat(filepath.Join(dst, filepath.FromSlash(es[rel].Dest))); err == nil &&
					!sameTime(st.ModTime(), testutil.MTime) {
					t.Errorf("%s mtime %v", rel, st.ModTime())
				}
			}
			if name == "android" && es["DCIM/Camera/IMG_20240102_101010.jpg"].Header != "mismatch" {
				t.Error("HEIC named .jpg not flagged mismatch")
			}
			noTmp(t, dst)
			if _, err := os.Stat(filepath.Join(dst, "Reports", "manifest.json")); err != nil {
				t.Fatal("manifest.json missing")
			}
		})
	}
}

func TestRescueFromFatImage(t *testing.T) {
	card := testutil.AndroidCard(true)
	img := testutil.FAT32Image(t, card, 64<<20)
	dst := filepath.Join(t.TempDir(), "out")
	r := &Rescue{From: img, Raw: true, Dest: dst, StallTimeout: 10 * time.Second, LookupDrive: testDrives(img, dst)}
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	checkCopies(t, card, dst, entries(t, dst))
}

func TestRerunAllAlreadyCopied(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	if _, err := do(t, newRescue(src, dst, newFake(t, src)), nil, nil); err != nil {
		t.Fatal(err)
	}
	f := newFake(t, src)
	res, err := do(t, newRescue(src, dst, f), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["ALREADY-COPIED"] != len(card) {
		t.Fatalf("counters %v", res.Counters)
	}
	// The scan may peek at the first 512 bytes of unknown-type files (one open each); nothing is
	// copied again.
	for name, n := range f.opens {
		if c := card[name].Cat; n > 1 || (n == 1 && c != profile.Other && !strings.HasPrefix(name, "LOST.DIR")) {
			t.Fatalf("re-run opened %s %d times", name, n)
		}
	}
}

func TestRerunFixesDeletedAndPartial(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.bad["DCIM/100CANON/IMG_0001.CR3"] = []faultreader.Range{{Off: 10000, Len: 5000}}
	if _, err := do(t, newRescue(src, dst, f), nil, nil); err != nil {
		t.Fatal(err)
	}
	es := entries(t, dst)
	if es["DCIM/100CANON/IMG_0001.CR3"].Status != manifest.PARTIAL || !strings.Contains(es["DCIM/100CANON/IMG_0001.CR3"].Dest, "__PARTIAL") {
		t.Fatalf("first run: %+v", es["DCIM/100CANON/IMG_0001.CR3"])
	}
	os.Remove(filepath.Join(dst, filepath.FromSlash(es["DCIM/100CANON/IMG_0001.JPG"].Dest)))
	if _, err := do(t, newRescue(src, dst, newFake(t, src)), nil, nil); err != nil {
		t.Fatal(err)
	}
	es = entries(t, dst)
	checkCopies(t, card, dst, es)
	if _, err := os.Stat(filepath.Join(dst, "Photos", "DCIM", "100CANON", "IMG_0001__PARTIAL.CR3")); !os.IsNotExist(err) {
		t.Fatal("old partial kept")
	}
}

func TestNeverReplaceGoodWithWorse(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	rel := "DCIM/100CANON/MVI_0002.MP4"
	later := testutil.MTime.Add(time.Hour)
	os.Chtimes(filepath.Join(src, filepath.FromSlash(rel)), later, later) // not "already copied" any more
	f := newFake(t, src)
	f.bad[rel] = []faultreader.Range{{Off: 0, Len: 20000}}
	do(t, newRescue(src, dst, f), nil, nil)
	es := entries(t, dst)
	if es[rel].Status != manifest.OK {
		t.Fatalf("good copy replaced: %+v", es[rel])
	}
	checkCopies(t, card, dst, es)
	if _, err := os.Stat(filepath.Join(dst, "Videos", "DCIM", "100CANON", "MVI_0002__PARTIAL.MP4")); !os.IsNotExist(err) {
		t.Fatal("worse copy kept next to the good one")
	}
}

func TestDiskFullProactive(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Free = func(string) (uint64, error) { return 1 << 20, nil }
	p := &prompter{answers: map[string]job.Choice{"short-space": job.Continue}}
	_, err := do(t, r, p, nil)
	if err == nil || p.asked["disk-full"] != 1 {
		t.Fatalf("err=%v asked=%v", err, p.asked)
	}
	noTmp(t, dst)
}

func TestDiskFullReactive(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	os.MkdirAll(dst, 0o755)
	full := &os.PathError{Op: "write", Path: "x", Err: diskFullErrno}
	p := &prompter{}
	_, err := do(t, newRescue(src, dst, newFake(t, src)), p, destfs.NewQuota(destfs.New(dst, src), 30_000, full))
	if err == nil || p.asked["disk-full"] != 1 {
		t.Fatalf("err=%v asked=%v", err, p.asked)
	}
	noTmp(t, dst)
}

func TestFileTooLargeIsNotDiskFull(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	base := r.LookupDrive
	r.LookupDrive = func(p string) (drives.Drive, error) {
		d, err := base(p)
		if p == dst {
			d.FS = "FAT32"
		}
		return d, err
	}
	p := &prompter{}
	env := job.Env{Prompt: p}
	plan, _ := r.Plan(context.Background(), env)
	r.selected[0].Size = 5 << 30 // as if the card held a 5 GB video
	if _, err := r.Run(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if p.asked["disk-full"] != 0 {
		t.Fatal("too-large treated as disk full")
	}
	if e := entries(t, dst)[r.selected[0].Rel]; e.Status != manifest.FAILED || !strings.Contains(e.Error, "4 GiB") {
		t.Fatalf("%+v", e)
	}
}

func TestStalledFileRetriedAtEnd(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	f.openErr["DCIM/100CANON/IMG_0001.JPG"] = []error{source.ErrStalled, nil}
	res, err := do(t, newRescue(src, dst, f), nil, nil)
	if err != nil || res.Counters["OK"] != len(card) || res.Counters["STALLED"] != 0 {
		t.Fatalf("%v %v", res.Counters, err)
	}
	checkCopies(t, card, dst, entries(t, dst))
}

func TestTooManyRestartsPromptsOnce(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	for rel := range card {
		f.openErr[rel] = []error{source.ErrTooManyRestarts}
	}
	p := &prompter{answers: map[string]job.Choice{"card-not-responding": SkipRest}}
	if _, err := do(t, newRescue(src, dst, f), p, nil); err != nil || p.asked["card-not-responding"] != 1 {
		t.Fatalf("err=%v asked=%v", err, p.asked)
	}
	for rel, e := range entries(t, dst) {
		if e.Status != manifest.FAILED {
			t.Errorf("%s: %s", rel, e.Status)
		}
	}
}

func TestLockedFileRetriedThenReported(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	rel := "DCIM/100CANON/IMG_0001.JPG"
	f.openErr[rel] = []error{source.ErrLocked}
	p := &prompter{}
	if _, err := do(t, newRescue(src, dst, f), p, nil); err != nil {
		t.Fatal(err)
	}
	if p.asked["files-locked"] != 1 || f.opens[rel] < 2 || entries(t, dst)[rel].Status != manifest.LOCKED {
		t.Fatalf("asked=%v opens=%d", p.asked, f.opens[rel])
	}
}

func TestUnplugResumesAtNewRoot(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	newRoot := testutil.WriteFolder(t, card) // the card came back as another drive letter
	f := newFake(t, src)
	r := newRescue(src, dst, f)
	polls := 0
	r.Present = func(drives.Identity, string) (string, bool) {
		f.mu.Lock()
		gone := f.gone
		f.mu.Unlock()
		if !gone {
			return f.root, true
		}
		if polls++; polls < 3 {
			return "", false
		}
		return newRoot, true
	}
	calls := 0
	f.opt.OnRead = func(off int64, n int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if calls++; calls == 5 && f.retargets == 0 { // pulled mid-copy of the second file
			f.gone = true
		}
		if f.gone {
			return errors.New("device not ready")
		}
		return nil
	}
	p := &prompter{}
	if _, err := do(t, r, p, nil); err != nil {
		t.Fatal(err)
	}
	if p.asked["card-removed"] != 1 || f.retargets != 1 {
		t.Fatalf("asked=%v retargets=%d", p.asked, f.retargets)
	}
	checkCopies(t, card, dst, entries(t, dst))
}

func TestUnplugTimeoutNonInteractive(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	r := newRescue(src, dst, f)
	r.WaitTimeout = 50 * time.Millisecond
	r.Present = func(drives.Identity, string) (string, bool) { return "", false }
	f.gone = true
	_, err := do(t, r, &prompter{}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("err = %v", err)
	}
}

func TestSourceFileVanishes(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	rel := "MISC/AUTPRINT.MRK"
	f.openErr[rel] = []error{&fs.PathError{Op: "open", Path: rel, Err: fs.ErrNotExist}}
	if _, err := do(t, newRescue(src, dst, f), nil, nil); err != nil {
		t.Fatal(err)
	}
	if e := entries(t, dst)[rel]; e.Status != manifest.FAILED || !strings.Contains(e.Error, "disappeared") {
		t.Fatalf("%+v", e)
	}
}

func TestDegradeMonitorFiresOnce(t *testing.T) {
	card := testutil.Card{}
	for i := 0; i < 5; i++ {
		card[strings.Repeat("d", i+1)+".jpg"] = testutil.File{Data: testutil.JPEG(1000, "x")}
	}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	for rel := range card {
		f.openErr[rel] = []error{errors.New("i/o error")}
	}
	p := &prompter{}
	do(t, newRescue(src, dst, f), p, nil)
	if p.asked["card-degrading"] != 1 {
		t.Fatalf("asked %v", p.asked)
	}
}

func TestRescueExcludedRowsReported(t *testing.T) {
	card := testutil.AndroidCard(false)
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Exclude = []string{"recommended"}
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	es := entries(t, dst)
	if e := es["DCIM/.thumbnails/t1.jpg"]; e.Status != manifest.EXCLUDED || e.Error != "thumbnails" {
		t.Fatalf("%+v", e)
	}
	if e := es["Android/data/com.x/cache.jpg"]; e.Status != manifest.OK {
		t.Fatalf("not-recommended suggestion applied: %+v", e)
	}
	csvs, _ := filepath.Glob(filepath.Join(dst, "Reports", "copy_*.csv"))
	b, _ := os.ReadFile(csvs[0])
	if !bytes.Contains(b, []byte("EXCLUDED")) {
		t.Fatal("CSV lacks EXCLUDED rows")
	}
}

func TestRescueDefaultIncludesOther(t *testing.T) {
	card := testutil.AndroidCard(false)
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	if e := entries(t, dst)["notes.ini"]; e.Status != manifest.OK || !strings.HasPrefix(e.Dest, "Other/") {
		t.Fatalf("%+v", e)
	}
}

func TestTypesFilterKeepsNames(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	r.Types = []profile.Category{profile.Photos}
	do(t, r, nil, nil)
	es := entries(t, dst)
	if e := es["DCIM/100CANON/IMG_0001.JPG"]; e.Status != manifest.OK || e.Dest != "Photos/DCIM/100CANON/IMG_0001.JPG" {
		t.Fatalf("%+v", e)
	}
	if e := es["DCIM/100CANON/MVI_0002.MP4"]; e.Status != manifest.EXCLUDED || !strings.Contains(e.Error, "type not selected") {
		t.Fatalf("%+v", e)
	}
}

func blocked(t *testing.T, r *Rescue, kind string) {
	t.Helper()
	plan, err := r.Plan(context.Background(), job.Env{})
	defer r.Close()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range plan.Issues {
		found = found || (is.Kind == kind && is.Severity == job.Block)
	}
	if !found {
		t.Fatalf("no %s block in %+v", kind, plan.Issues)
	}
	if _, err := r.Run(context.Background(), job.Env{}, plan); err == nil {
		t.Fatal("Run started despite a Block")
	}
}

func TestRefusesDestInsideSource(t *testing.T) {
	src := testutil.WriteFolder(t, testutil.CameraCard())
	dst := filepath.Join(src, "rescue")
	blocked(t, newRescue(src, dst, newFake(t, src)), "dest-inside-source")
}

func TestRefusesForeignNonEmptyDest(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), t.TempDir()
	os.WriteFile(filepath.Join(dst, "taxes.xlsx"), []byte("x"), 0o644)
	blocked(t, newRescue(src, dst, newFake(t, src)), "dest-foreign")
}

func TestDestOfOtherCardRefused(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	os.MkdirAll(filepath.Join(dst, "Reports"), 0o755)
	s, _ := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	m, _ := manifest.Open(s)
	m.Volume = drives.Identity{FSSerial: "0000-0001", Model: "Another card"}
	m.SaveMeta()
	s.Close()
	blocked(t, newRescue(src, dst, newFake(t, src)), "dest-other-card")
}

func TestConcurrentRunRefused(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	r := newRescue(src, dst, newFake(t, src))
	plan, _ := r.Plan(context.Background(), job.Env{})
	defer r.Close()
	os.MkdirAll(filepath.Join(dst, "Reports"), 0o755)
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := r.Run(context.Background(), job.Env{}, plan); !errors.Is(err, job.ErrBusy) {
		t.Fatalf("err = %v", err)
	}
}

// fakeClock advances only when the fault reader "sleeps".
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func TestSlowErrorsHitFileBudgetAndAsk(t *testing.T) {
	card := testutil.Card{"a.jpg": {Data: testutil.JPEG(200_000, "a")}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	clk := &fakeClock{t: time.Unix(0, 0)}
	f.opt = faultreader.Options{FailDelay: 25 * time.Second, Sleep: clk.sleep}
	f.bad["a.jpg"] = []faultreader.Range{{Off: 50_000, Len: 100_000}}
	r := newRescue(src, dst, f)
	r.Now = clk.now
	p := &prompter{answers: map[string]job.Choice{"file-slow": job.Skip}}
	if _, err := do(t, r, p, nil); err != nil {
		t.Fatal(err)
	}
	if p.asked["file-slow"] != 1 {
		t.Fatalf("asked %v", p.asked)
	}
	if e := entries(t, dst)["a.jpg"]; e.Status != manifest.PARTIAL {
		t.Fatalf("%+v", e)
	}
}

func TestSlowHealthyReadsNeverBudgeted(t *testing.T) {
	card := testutil.Card{"a.jpg": {Data: testutil.JPEG(3<<20, "a")}}
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	f := newFake(t, src)
	clk := &fakeClock{t: time.Unix(0, 0)}
	f.opt = faultreader.Options{Delay: 10 * time.Minute, Sleep: clk.sleep} // very slow but healthy
	r := newRescue(src, dst, f)
	r.Now = clk.now
	p := &prompter{}
	if _, err := do(t, r, p, nil); err != nil {
		t.Fatal(err)
	}
	if p.asked["file-slow"] != 0 || entries(t, dst)["a.jpg"].Status != manifest.OK {
		t.Fatalf("asked %v", p.asked)
	}
}

const envChild = "AIO_RESCUE_CHILD"

// childRescue runs a slow rescue and is killed by the parent mid-run.
func childRescue() {
	parts := strings.SplitN(os.Getenv(envChild), "|", 2)
	src, dst := parts[0], parts[1]
	f := newFake(nil, src)
	f.opt = faultreader.Options{Delay: 20 * time.Millisecond}
	r := newRescue(src, dst, f)
	r.LookupDrive = testDrives(src, dst)
	env := job.Env{Prompt: &prompter{}, Progress: func(p job.Progress) {
		if p.Done > 0 {
			os.Stdout.WriteString("progress\n")
		}
	}}
	plan, _ := r.Plan(context.Background(), env)
	r.Run(context.Background(), env, plan)
	os.Exit(0)
}

func TestResumeAfterKill(t *testing.T) {
	card := testutil.AndroidCard(false)
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envChild+"="+src+"|"+dst)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	for i := 0; i < 3 && sc.Scan(); i++ {
	}
	cmd.Process.Kill()
	cmd.Wait()
	res, err := do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["ALREADY-COPIED"] == 0 {
		t.Fatalf("nothing kept from the killed run: %v", res.Counters)
	}
	checkCopies(t, card, dst, entries(t, dst))
	noTmp(t, dst)
}

var _ = syscall.Errno(0)

func TestRescueCompressedMatchesFolder(t *testing.T) {
	card := testutil.AndroidCard(false)
	src := testutil.WriteFolder(t, card)
	plain, zipped := filepath.Join(t.TempDir(), "plain"), filepath.Join(t.TempDir(), "zipped")
	do(t, newRescue(src, plain, newFake(t, src)), nil, nil)
	r := newRescue(src, zipped, newFake(t, src))
	r.Compress = true
	if _, err := do(t, r, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, b := entries(t, plain), entries(t, zipped)
	for rel := range card {
		if a[rel].SHA256 == "" || a[rel].SHA256 != b[rel].SHA256 || b[rel].Status != manifest.OK || !strings.Contains(b[rel].Dest, ".zip/") {
			t.Errorf("%s: plain %+v zipped %+v", rel, a[rel], b[rel])
		}
	}
	// re-run: everything already stored in closed parts
	r2 := newRescue(src, zipped, newFake(t, src))
	r2.Compress = true
	res, err := do(t, r2, nil, nil)
	if err != nil || res.Counters["ALREADY-COPIED"] != len(card) {
		t.Fatalf("%v %v", res.Counters, err)
	}
}

func TestFreshRecopiesEverything(t *testing.T) {
	card := testutil.CameraCard()
	src, dst := testutil.WriteFolder(t, card), filepath.Join(t.TempDir(), "out")
	do(t, newRescue(src, dst, newFake(t, src)), nil, nil)
	r := newRescue(src, dst, newFake(t, src))
	r.Fresh = true
	res, err := do(t, r, nil, nil)
	if err != nil || res.Counters["ALREADY-COPIED"] != 0 || res.Counters["OK"] != len(card) {
		t.Fatalf("%v %v", res.Counters, err)
	}
}
