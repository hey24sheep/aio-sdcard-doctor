package recovery

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

const envWorker = "AIO_RECOVERY_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(envWorker) == "1" {
		os.Exit(worker.Main())
	}
	worker.Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1")
		return c
	}
	os.Exit(m.Run())
}

// realJPEG is a decodable JPEG of about n bytes (noise compresses badly).
func realJPEG(t testing.TB, w, h int, seed byte) []byte {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	v := uint32(seed) + 1
	for i := range m.Pix {
		v = v*1103515245 + 12345
		m.Pix[i] = byte(v >> 16)
	}
	for i := 3; i < len(m.Pix); i += 4 {
		m.Pix[i] = 255
	}
	m.Set(0, 0, color.White)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func env() job.Env {
	return job.Env{Prompt: job.AutoPrompter{Yes: true}, Progress: func(job.Progress) {}, Now: time.Now, Log: func(string, string) {}}
}

func run(t *testing.T, r *Recover) (job.Plan, job.Result) {
	t.Helper()
	if r.StallTimeout == 0 {
		r.StallTimeout = 5 * time.Second
	}
	defer r.Close()
	p, err := r.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range p.Issues {
		if is.Severity == job.Block {
			t.Fatalf("blocked: %s — %s", is.Title, is.Detail)
		}
	}
	res, err := r.Run(context.Background(), env(), p)
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

func entries(t *testing.T, dst string) map[string]manifest.Entry {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(dst, "Reports", "recover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := manifest.Open(s)
	out := map[string]manifest.Entry{}
	m.Each(func(e manifest.Entry) error { out[e.Source] = e; return nil })
	return out
}

type cardImg struct {
	path       string
	gone, keep []byte
}

// card: KEEP.JPG live, GONE.JPG deleted (recoverable), BAD.JPG deleted with a broken body.
func card(t *testing.T) cardImg {
	t.Helper()
	gone, keep := realJPEG(t, 120, 90, 1), realJPEG(t, 100, 80, 2)
	bad := append(append([]byte{}, realJPEG(t, 60, 40, 3)[:600]...), make([]byte, 4000)...) // truncated jpeg
	img := testutil.FAT32Image(t, testutil.Card{
		"DCIM/KEEP.JPG": {Data: keep}, "DCIM/GONE.JPG": {Data: gone}, "DCIM/BAD.JPG": {Data: bad},
	}, 64<<20)
	testutil.DeleteFATFile(t, img, "GONE    JPG")
	testutil.DeleteFATFile(t, img, "BAD     JPG")
	return cardImg{path: img, gone: gone, keep: keep}
}

func TestExtractValidatedOK(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	_, res := run(t, &Recover{From: c.path, Dest: dst})
	got, err := os.ReadFile(filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG"))
	if err != nil || !bytes.Equal(got, c.gone) {
		t.Fatalf("recovered file: %v (%d bytes)", err, len(got))
	}
	if e := entries(t, dst)["DCIM/_ONE.JPG"]; e.Status != manifest.DELETED_RECOVERED {
		t.Fatalf("entry %+v", e)
	}
	if res.Counters[string(manifest.DELETED_RECOVERED)] != 1 {
		t.Fatalf("counters %v", res.Counters)
	}
}

func TestExtractValidationFailFlagged(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	_, res := run(t, &Recover{From: c.path, Dest: dst})
	if _, err := os.Stat(filepath.Join(dst, "Undeleted", "DCIM", "_AD.JPG")); err == nil {
		t.Fatal("a damaged file was written")
	}
	e := entries(t, dst)["DCIM/_AD.JPG"]
	if e.Status != manifest.DELETED_UNRECOVERABLE || !strings.Contains(e.Error, "jpeg") {
		t.Fatalf("entry %+v", e)
	}
	if res.Clean {
		t.Fatal("a run with an unrecoverable file reported clean")
	}
	// the CSV lists it with its path and reason
	csvs, _ := filepath.Glob(filepath.Join(dst, "Reports", "recover_2*.csv"))
	b, _ := os.ReadFile(csvs[0])
	if len(csvs) != 1 || !strings.Contains(string(b), "DCIM/_AD.JPG") || !strings.Contains(string(b), "DELETED-UNRECOVERABLE") {
		t.Fatalf("csv %v:\n%s", csvs, b)
	}
}

func TestUnrecoverableOnlyFlagged(t *testing.T) {
	c := card(t)
	img, _ := os.ReadFile(c.path)
	// GONE's record now points at KEEP's first cluster: its space was reused
	keepRec := recordOf(t, img, "KEEP    JPG")
	goneRec := recordOf(t, img, "\xE5ONE    JPG")
	copy(goneRec[20:22], keepRec[20:22])
	copy(goneRec[26:28], keepRec[26:28])
	copy(goneRec[28:32], keepRec[28:32]) // and the same length: all of its clusters are taken
	os.WriteFile(c.path, img, 0o644)
	dst := filepath.Join(t.TempDir(), "out")
	r := &Recover{From: c.path, Dest: dst}
	run(t, r)
	if _, err := os.Stat(filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")); err == nil {
		t.Fatal("an overwritten file was written")
	}
	e := entries(t, dst)["DCIM/_ONE.JPG"]
	if e.Status != manifest.DELETED_UNRECOVERABLE || !strings.Contains(e.Error, "DCIM/KEEP.JPG") {
		t.Fatalf("entry %+v", e)
	}
}

func recordOf(t *testing.T, img []byte, short string) []byte {
	for i := 0; ; {
		j := bytes.Index(img[i:], []byte(short))
		if j < 0 {
			t.Fatalf("no record %q", short)
		}
		if (i+j)%32 == 0 {
			return img[i+j : i+j+32]
		}
		i += j + 1
	}
}

func TestOverwrittenByPath(t *testing.T) {
	rows := []Row{{Kind: "deleted", Path: "a.jpg", Extents: [][2]int64{{1000, 3000}}, internal: "overwritten"},
		{Kind: "deleted", Path: "b.jpg", Extents: [][2]int64{{8000, 2000}}, internal: "fragmented-unknown"}}
	live := []liveRun{{"DCIM/x.jpg", 0, 5000}, {"DCIM/y.jpg", 9000, 512}}
	annotate(rows, live)
	if !strings.Contains(rows[0].Comment, "DCIM/x.jpg") || !strings.Contains(rows[1].Comment, "DCIM/y.jpg") {
		t.Fatalf("%q / %q", rows[0].Comment, rows[1].Comment)
	}
}

func TestStateMapping(t *testing.T) {
	for _, c := range []struct{ in, state, comment string }{
		{"recoverable", Excellent, "No overwritten clusters detected"},
		{"fragmented-unknown", Poor, "may come back damaged"},
		{"overwritten", Unrecoverable, "verwritten"},
		{"metadata-damaged", Unrecoverable, "Directory entry damaged"},
	} {
		st, cm := mapState(c.in)
		if st != c.state || !strings.Contains(cm, c.comment) {
			t.Errorf("%s → %s %q", c.in, st, cm)
		}
	}
	if st, cm := carvedState(false); st != Unrecoverable || !strings.Contains(cm, "truncated") {
		t.Errorf("incomplete carve → %s %q", st, cm)
	}
}

func TestExtractSkipsRescuedDuplicates(t *testing.T) {
	c := card(t)
	// a previous rescue already holds GONE.JPG's content
	rescueDir := t.TempDir()
	sum := sha256.Sum256(c.gone)
	man := filepath.Join(rescueDir, "manifest.json")
	writeManifestJSON(t, man, strings.ToUpper(hex.EncodeToString(sum[:])))
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst, SkipRescued: man})
	if _, err := os.Stat(filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")); err == nil {
		t.Fatal("a duplicate of a rescued file was kept")
	}
	if e := entries(t, dst)["DCIM/_ONE.JPG"]; e.Status != manifest.DUPLICATE {
		t.Fatalf("entry %+v", e)
	}
}

func writeManifestJSON(t *testing.T, path, sha string) {
	j := `{"Version":1,"Entries":[{"Source":"x/GONE.JPG","Status":"OK","SHA256":"` + sha + `"}]}`
	if err := os.WriteFile(path, []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractResume(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst})
	f := filepath.Join(dst, "Undeleted", "DCIM", "_ONE.JPG")
	st1, _ := os.Stat(f)
	time.Sleep(20 * time.Millisecond)
	_, res := run(t, &Recover{From: c.path, Dest: dst})
	st2, _ := os.Stat(f)
	if res.Counters[string(manifest.ALREADY_COPIED)] != 1 || !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatalf("second run counters %v (file rewritten: %v)", res.Counters, !st1.ModTime().Equal(st2.ModTime()))
	}
}

func TestRestoreFolderStructureOff(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst, Flat: true})
	if got, err := os.ReadFile(filepath.Join(dst, "Undeleted", "Photos", "_ONE.JPG")); err != nil || !bytes.Equal(got, c.gone) {
		t.Fatalf("flat layout: %v", err)
	}
}

func TestRecoverCompressed(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: c.path, Dest: dst, Compress: true})
	parts, _ := filepath.Glob(filepath.Join(dst, "Undeleted-*.zip"))
	if len(parts) != 1 {
		t.Fatalf("zip parts %v", parts)
	}
	z, err := zip.OpenReader(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if len(z.File) != 1 || !strings.HasSuffix(z.File[0].Name, "_ONE.JPG") {
		t.Fatalf("zip holds %v", z.File)
	}
}

func TestRecoverListCSV(t *testing.T) {
	c := card(t)
	dst := filepath.Join(t.TempDir(), "out")
	r := &Recover{From: c.path, Dest: dst, List: true}
	_, res := run(t, r)
	lists, _ := filepath.Glob(filepath.Join(dst, "Reports", "recover_list_*.csv"))
	if len(lists) != 1 {
		t.Fatalf("lists %v (reports %v)", lists, res.Reports)
	}
	b, _ := os.ReadFile(lists[0])
	for _, want := range []string{"State", "Excellent", "DCIM/_ONE.JPG", "DCIM/_AD.JPG"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("list lacks %q:\n%s", want, b)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "Undeleted")); err == nil {
		t.Fatal("--list extracted files")
	}
}

// TestDeepScanOnFatImage: a file whose directory record is destroyed is carved out of free space;
// live files are skipped (not reported as carved).
func TestDeepScanOnFatImage(t *testing.T) {
	lost, keep := realJPEG(t, 140, 100, 7), realJPEG(t, 100, 80, 2)
	img := testutil.FAT32Image(t, testutil.Card{"DCIM/KEEP.JPG": {Data: keep}, "LOST.JPG": {Data: lost}}, 64<<20)
	testutil.DeleteFATFile(t, img, "LOST    JPG")
	b, _ := os.ReadFile(img)
	rec := recordOf(t, b, "\xE5OST    JPG")
	copy(rec[1:11], "XXXXXXXXXX")
	for i := 20; i < 32; i++ {
		rec[i] = 0 // no cluster, no size: undelete can't find it
	}
	os.WriteFile(img, b, 0o644)
	dst := filepath.Join(t.TempDir(), "out")
	_, res := run(t, &Recover{From: img, Dest: dst, Deep: true})
	carved, _ := filepath.Glob(filepath.Join(dst, "Carved", "Photos", "recovered_*.jpg"))
	if len(carved) != 1 {
		t.Fatalf("carved %v, counters %v", carved, res.Counters)
	}
	if got, _ := os.ReadFile(carved[0]); !bytes.Equal(got, lost) {
		t.Fatal("carved bytes differ")
	}
}

func TestCarvedDamagedOnlyFlagged(t *testing.T) {
	trunc := realJPEG(t, 140, 100, 9)
	trunc = append(trunc[:len(trunc)/2:len(trunc)/2], make([]byte, 20000)...) // no end marker before free space ends
	img := testutil.FAT32Image(t, testutil.Card{"T.JPG": {Data: trunc}}, 64<<20)
	testutil.DeleteFATFile(t, img, "T       JPG")
	dst := filepath.Join(t.TempDir(), "out")
	run(t, &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true})
	if m, _ := filepath.Glob(filepath.Join(dst, "Carved", "*", "*")); len(m) != 0 {
		t.Fatalf("damaged carve written: %v", m)
	}
	found := false
	for _, e := range entries(t, dst) {
		found = found || e.Status == manifest.CARVED_DAMAGED
	}
	if !found {
		t.Fatalf("no CARVED-DAMAGED row: %v", entries(t, dst))
	}
}

func TestDeepScanResume(t *testing.T) {
	lost := realJPEG(t, 140, 100, 7)
	img := testutil.FAT32Image(t, testutil.Card{"LOST.JPG": {Data: lost}}, 64<<20)
	testutil.DeleteFATFile(t, img, "LOST    JPG")
	dst := filepath.Join(t.TempDir(), "out")
	r := &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true, List: true}
	run(t, r)
	n1 := len(r.Rows())
	r2 := &Recover{From: img, Dest: dst, Deep: true, NoUndelete: true, List: true}
	run(t, r2)
	if n1 == 0 || len(r2.Rows()) != n1 || !r2.resumedIndex {
		t.Fatalf("rows %d then %d (resumed %v)", n1, len(r2.Rows()), r2.resumedIndex)
	}
}

func TestUndeleteDisabledShowsReason(t *testing.T) {
	img := testutil.FAT32Image(t, testutil.Card{"A.JPG": {Data: realJPEG(t, 40, 40, 1)}}, 64<<20)
	r := &Recover{From: img, Dest: filepath.Join(t.TempDir(), "out"), StallTimeout: 5 * time.Second}
	p, err := r.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	names := map[string]job.Capability{}
	for _, c := range p.Caps {
		names[c.Name] = c
	}
	if !names["Undelete"].Enabled || !names["Deep scan"].Enabled {
		t.Fatalf("caps %+v", p.Caps)
	}
	// a folder source (no raw access): undelete greyed with the reason, deep scan needs the device
	r2 := &Recover{From: t.TempDir(), Dest: filepath.Join(t.TempDir(), "out"), StallTimeout: 5 * time.Second}
	p2, err := r2.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	blocked := false
	for _, is := range p2.Issues {
		blocked = blocked || is.Severity == job.Block && strings.Contains(is.Detail, "device")
	}
	if !blocked {
		t.Fatalf("folder source: issues %+v", p2.Issues)
	}
}

// TestHostileNamesStayInside: names from a damaged or crafted card can't escape Undeleted/, and
// two deleted files that read back with the same name get different destinations.
func TestHostileNamesStayInside(t *testing.T) {
	r := &Recover{Dest: t.TempDir()}
	rows := dedupe([]Row{
		{Kind: "deleted", Path: "../../evil.jpg", Name: "evil.jpg", Cat: "Photos"},
		{Kind: "deleted", Path: "DCIM/a:b?.jpg", Name: "a:b?.jpg", Cat: "Photos"},
		{Kind: "deleted", Path: "DCIM/_ONE.JPG", Name: "_ONE.JPG", Cat: "Photos"},
		{Kind: "deleted", Path: "DCIM/_ONE.JPG", Name: "_ONE.JPG", Cat: "Photos"},
	})
	dests := r.plan(rows)
	seen := map[string]bool{}
	for i, d := range dests {
		if !strings.HasPrefix(d, "Undeleted/") || strings.Contains(d, "..") || strings.ContainsAny(d[len("Undeleted/"):], `:?\`) {
			t.Errorf("row %d → %q", i, d)
		}
		if seen[d] {
			t.Errorf("two rows share %q", d)
		}
		seen[d] = true
	}
	if rows[3].ID == rows[2].ID || r.key(rows[3]) == r.key(rows[2]) {
		t.Fatalf("duplicate ids: %q %q", rows[2].ID, rows[3].ID)
	}
}
