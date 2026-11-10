package verify

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func sha(b []byte) string { return fmt.Sprintf("%X", sha256.Sum256(b)) }

var files = map[string][]byte{
	"DCIM/a.jpg":     []byte("photo a"),
	"DCIM/b.jpg":     []byte("photo b!"),
	"Music/s.mp3":    []byte("song"),
	"Docs/empty.txt": {},
}

func dests() map[string]string {
	return map[string]string{"DCIM/a.jpg": "Photos/DCIM/a.jpg", "DCIM/b.jpg": "Photos/DCIM/b.jpg",
		"Music/s.mp3": "Music/Music/s.mp3", "Docs/empty.txt": "Documents/Docs/empty.txt"}
}

// rescueFolder writes a rescue output (files + Reports/manifest.json) and returns its root.
func rescueFolder(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Reports"), 0o755)
	s, _ := job.OpenStore(filepath.Join(root, "Reports", "state.db"))
	m, _ := manifest.Open(s)
	for src, data := range files {
		d := dests()[src]
		p := filepath.Join(root, filepath.FromSlash(d))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
		m.Put(manifest.Entry{Source: src, Dest: d, Bytes: int64(len(data)), SHA256: sha(data), Status: manifest.OK})
	}
	m.Put(manifest.Entry{Source: "gone.jpg", Status: manifest.EXCLUDED})
	m.ExportJSON(destfs.New(root), filepath.Join(root, "Reports", "manifest.json"))
	s.Close()
	return root
}

func ntfs(string) (drives.Drive, error) { return drives.Drive{FS: "NTFS"}, nil }

func do(t *testing.T, v *Verify) (job.Result, job.Plan, error) {
	t.Helper()
	if v.LookupDrive == nil {
		v.LookupDrive = ntfs
	}
	env := job.Env{Prompt: job.AutoPrompter{}}
	p, err := v.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Run(context.Background(), env, p)
	return res, p, err
}

func summary(t *testing.T, dir string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "verify_summary_*.txt"))
	if len(m) == 0 {
		t.Fatal("no summary")
	}
	b, _ := os.ReadFile(m[len(m)-1])
	return string(b)
}

func TestVerifyCleanAllOK(t *testing.T) {
	root := rescueFolder(t)
	res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json")})
	if err != nil || res.Counters["OK"] != len(files) || !res.Clean {
		t.Fatalf("%v %v", res.Counters, err)
	}
	if !strings.Contains(summary(t, filepath.Join(root, "Reports")), "ALL FILES MATCH") {
		t.Fatal("no ALL FILES MATCH")
	}
}

func TestVerifyDetectsMissingCorruptWrongSize(t *testing.T) {
	root := rescueFolder(t)
	os.Remove(filepath.Join(root, "Photos", "DCIM", "a.jpg"))
	os.WriteFile(filepath.Join(root, "Photos", "DCIM", "b.jpg"), []byte("photo B!"), 0o644)
	os.WriteFile(filepath.Join(root, "Music", "Music", "s.mp3"), []byte("longer song"), 0o644)
	res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counters["MISSING"] != 1 || res.Counters["CORRUPT"] != 1 || res.Counters["WRONG-SIZE"] != 1 || res.Clean {
		t.Fatalf("%v", res.Counters)
	}
}

// TestVerifyResume: an interrupted verify continues; rows that were not OK are checked again.
func TestVerifyResume(t *testing.T) {
	root := rescueFolder(t)
	mf := filepath.Join(root, "Reports", "manifest.json")
	v := &Verify{Manifest: mf, LookupDrive: ntfs}
	if _, err := v.Plan(context.Background(), job.Env{}); err != nil {
		t.Fatal(err)
	}
	s, _ := job.OpenStore(v.storePath())
	job.Put(s, v.bucket(), "Photos/DCIM/a.jpg", row{Status: "OK", Path: "Photos/DCIM/a.jpg"})      // done earlier
	job.Put(s, v.bucket(), "Photos/DCIM/b.jpg", row{Status: "MISSING", Path: "Photos/DCIM/b.jpg"}) // was missing, now back
	s.Close()
	os.Remove(filepath.Join(root, "Photos", "DCIM", "a.jpg")) // checked already: not re-read
	res, p, err := do(t, &Verify{Manifest: mf})
	if err != nil || p.Resume == nil || res.Counters["OK"] != len(files) {
		t.Fatalf("resume %+v counters %v err %v", p.Resume, res.Counters, err)
	}
	res, _, _ = do(t, &Verify{Manifest: mf, Restart: true})
	if res.Counters["MISSING"] != 1 {
		t.Fatalf("restart didn't re-check: %v", res.Counters)
	}
}

// TestVerifyAgainAfterFinishing: a finished verify never answers from its cache (I9).
func TestVerifyAgainAfterFinishing(t *testing.T) {
	root := rescueFolder(t)
	mf := filepath.Join(root, "Reports", "manifest.json")
	if res, _, _ := do(t, &Verify{Manifest: mf}); !res.Clean {
		t.Fatal("first run not clean")
	}
	os.WriteFile(filepath.Join(root, "Photos", "DCIM", "b.jpg"), []byte("photo B!"), 0o644)
	res, p, _ := do(t, &Verify{Manifest: mf})
	if p.Resume != nil || res.Counters["CORRUPT"] != 1 {
		t.Fatalf("second run reused the finished run: resume %+v counters %v", p.Resume, res.Counters)
	}
}

func TestVerifyFAT32Warning(t *testing.T) {
	root := rescueFolder(t)
	v := &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json"),
		LookupDrive: func(string) (drives.Drive, error) { return drives.Drive{FS: "FAT32"}, nil }, MaxFile: 5}
	_, p, _ := do(t, v)
	found := false
	for _, is := range p.Issues {
		found = found || is.Kind == "fat-target"
	}
	if !found {
		t.Fatalf("%+v", p.Issues)
	}
}

func TestVerifyLegacyCSV(t *testing.T) {
	card := t.TempDir() // a new card the old PS rescue was copied to (contents of Recovered\ at its root)
	reports := filepath.Join(t.TempDir(), "Reports")
	os.MkdirAll(reports, 0o755)
	var rows []string
	for src, data := range files {
		d := dests()[src]
		p := filepath.Join(card, filepath.FromSlash(d))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
		rows = append(rows, fmt.Sprintf(`"OK","x","%s","Recovered\%s","%d","0","ok","%s",""`, src, strings.ReplaceAll(d, "/", `\`), len(data), sha(data)))
	}
	csv := "\xef\xbb\xbf\"Status\",\"Category\",\"Source\",\"Dest\",\"Bytes\",\"BadBytes\",\"Header\",\"SHA256\",\"Error\"\n" + strings.Join(rows, "\n") + "\n"
	os.WriteFile(filepath.Join(reports, "copy_20261109_213618_445.csv"), []byte(csv), 0o644)
	res, _, err := do(t, &Verify{Manifest: filepath.Join(reports, "copy_20261109_213618_445.csv"), Target: card})
	if err != nil || res.Counters["OK"] != len(files) {
		t.Fatalf("%v %v", res.Counters, err)
	}
	if m, _ := filepath.Glob(filepath.Join(card, "verify_*")); len(m) != 0 {
		t.Fatal("report written on the target")
	}
}

func zipRescue(t *testing.T) string {
	root := rescueFolder(t)
	byCat := map[string]map[string][]byte{}
	for src, d := range dests() {
		cat, rest, _ := strings.Cut(d, "/")
		if byCat[cat] == nil {
			byCat[cat] = map[string][]byte{}
		}
		byCat[cat][rest] = files[src]
		os.RemoveAll(filepath.Join(root, cat))
	}
	s, _ := job.OpenStore(filepath.Join(root, "Reports", "state.db"))
	m, _ := manifest.Open(s)
	for cat, ents := range byCat {
		f, _ := os.Create(filepath.Join(root, cat+"-0001.zip"))
		zw := zip.NewWriter(f)
		for name, data := range ents {
			w, _ := zw.Create(name)
			w.Write(data)
		}
		zw.Close()
		f.Close()
	}
	for src, d := range dests() {
		cat, rest, _ := strings.Cut(d, "/")
		e, _, _ := m.Get(src)
		e.Dest = cat + "-0001.zip/" + rest
		m.Put(e)
	}
	m.ExportJSON(destfs.New(root), filepath.Join(root, "Reports", "manifest.json"))
	s.Close()
	return root
}

func TestVerifyZipParts(t *testing.T) {
	root := zipRescue(t)
	res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json")})
	if err != nil || res.Counters["OK"] != len(files) {
		t.Fatalf("%v %v", res.Counters, err)
	}
}

func TestVerifyExtractedZip(t *testing.T) {
	root := zipRescue(t)
	target := t.TempDir() // the parts extracted into one folder
	for src, d := range dests() {
		_, rest, _ := strings.Cut(d, "/")
		p := filepath.Join(target, filepath.FromSlash(rest))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, files[src], 0o644)
	}
	res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json"), Target: target})
	if err != nil || res.Counters["OK"] != len(files) {
		t.Fatalf("%v %v", res.Counters, err)
	}
}

func TestVerifyImage(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	data := []byte(strings.Repeat("sector", 10000))
	os.WriteFile(img, data, 0o644)
	os.WriteFile(img+".json", []byte(fmt.Sprintf(`{"Complete":true,"SHA256":"%s","Size":%d}`, sha(data), len(data))), 0o644)
	res, _, err := do(t, &Verify{Image: img})
	if err != nil || !res.Clean {
		t.Fatalf("%v %v", res.Counters, err)
	}
	data[5] ^= 1
	os.WriteFile(img, data, 0o644)
	if res, _, _ = do(t, &Verify{Image: img}); res.Clean || res.Counters["CORRUPT"] != 1 {
		t.Fatalf("corrupt image passed: %v", res.Counters)
	}
	os.WriteFile(img+".json", []byte(`{"Complete":false}`), 0o644)
	v := &Verify{Image: img, LookupDrive: ntfs}
	p, _ := v.Plan(context.Background(), job.Env{})
	blocked := false
	for _, is := range p.Issues {
		blocked = blocked || is.Severity == job.Block
	}
	if !blocked {
		t.Fatal("incomplete image not refused")
	}
}

func TestVerifyReportsElsewhere(t *testing.T) {
	root := rescueFolder(t)
	elsewhere := t.TempDir()
	res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json"), Reports: elsewhere})
	if err != nil || !res.Clean {
		t.Fatal(res.Counters, err)
	}
	if m, _ := filepath.Glob(filepath.Join(elsewhere, "verify_summary_*.txt")); len(m) != 1 {
		t.Fatal("reports not written to the chosen folder")
	}
	if m, _ := filepath.Glob(filepath.Join(root, "Reports", "verify*")); len(m) != 0 {
		t.Fatalf("wrote next to the manifest anyway: %v", m)
	}
}
