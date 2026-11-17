package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
)

// withEntries adds report entries to a rescue folder's manifest.
func withEntries(t *testing.T, root string, es ...manifest.Entry) {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(root, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := manifest.Open(s)
	for _, e := range es {
		m.Put(e)
	}
	m.ExportJSON(destfs.New(root), filepath.Join(root, "Reports", "manifest.json"))
}

// OR9/SR7: a verify whose files all matched said Clean (exit 0, "Done") although the report
// had files that failed to copy, or copies saved with unreadable parts. Only files the user
// chose to leave out keep a verify clean.
func TestVerifyNotCleanWhenFilesFailedOrPartial(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    manifest.Entry
	}{
		{"failed", manifest.Entry{Source: "bad.jpg", Status: manifest.FAILED, Error: "I/O error"}},
		{"stalled", manifest.Entry{Source: "slow.jpg", Status: manifest.STALLED}},
		{"unreadable", manifest.Entry{Source: "dead.jpg", Status: manifest.UNREADABLE}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := rescueFolder(t)
			withEntries(t, root, tc.e)
			res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json")})
			if err != nil {
				t.Fatal(err)
			}
			if res.Clean {
				t.Errorf("clean with a file that was never copied (%s)", tc.e.Status)
			}
			if s := summary(t, filepath.Join(root, "Reports")); !strings.Contains(s, "not whole") {
				t.Errorf("summary doesn't say the copy isn't whole:\n%s", s)
			}
		})
	}
	t.Run("partial", func(t *testing.T) {
		root := rescueFolder(t)
		data := []byte("half a photo\x00\x00")
		d := "Photos/DCIM/c__PARTIAL.jpg"
		os.WriteFile(filepath.Join(root, filepath.FromSlash(d)), data, 0o644)
		withEntries(t, root, manifest.Entry{Source: "DCIM/c.jpg", Dest: d, Bytes: int64(len(data)), SHA256: sha(data), Status: manifest.PARTIAL, BadBytes: 2})
		res, _, err := do(t, &Verify{Manifest: filepath.Join(root, "Reports", "manifest.json")})
		if err != nil || res.Clean {
			t.Errorf("clean %v, %v: a copy with unreadable parts isn't whole", res.Clean, err)
		}
	})
}

// SR7: an image finished with unreadable areas (zeros in the image) verified as clean.
func TestVerifyImageWithUnreadableAreasNotClean(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	data := []byte(strings.Repeat("sector", 10000))
	os.WriteFile(img, data, 0o644)
	os.WriteFile(img+".json", []byte(fmt.Sprintf(`{"Complete":true,"SHA256":"%s","Size":%d,"BadBytes":4096}`, sha(data), len(data))), 0o644)
	res, p, err := do(t, &Verify{Image: img})
	if err != nil || res.Clean || res.Counters["OK"] != 1 {
		t.Errorf("clean %v, counters %v, %v; want the match counted, not clean", res.Clean, res.Counters, err)
	}
	if !strings.Contains(strings.Join(p.Summary, "\n"), "4096 bytes were unreadable when imaged") {
		t.Errorf("plan summary %q", p.Summary)
	}
}
