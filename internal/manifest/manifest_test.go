package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func open(t *testing.T, p string) (*job.Store, *Manifest) {
	t.Helper()
	s, err := job.OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestPutGetEachAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	s, m := open(t, p)
	m.Volume = drives.Identity{FSSerial: "AB12-CD34", Model: "SD"}
	m.SourceKind = "folder"
	if err := m.SaveMeta(); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"DCIM/b.jpg", "DCIM/a.jpg"} {
		if err := m.Put(Entry{Source: src, Status: OK, Bytes: 5}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, m = open(t, p)
	defer s.Close()
	if m.Volume.FSSerial != "AB12-CD34" || m.SourceKind != "folder" || m.Version != Version {
		t.Fatalf("meta %+v", m)
	}
	e, ok, err := m.Get("DCIM/a.jpg")
	if err != nil || !ok || e.Bytes != 5 {
		t.Fatalf("%+v %v %v", e, ok, err)
	}
	var order []string
	m.Each(func(e Entry) error { order = append(order, e.Source); return nil })
	if len(order) != 2 || order[0] != "DCIM/a.jpg" {
		t.Fatalf("each %v", order)
	}
	if n, _ := m.Count(); n != 2 {
		t.Fatalf("count %d", n)
	}
}

func TestExportJSONAtomic(t *testing.T) {
	dir := t.TempDir()
	s, m := open(t, filepath.Join(dir, "state.db"))
	defer s.Close()
	m.Put(Entry{Source: "x.jpg", Status: PARTIAL, BadBytes: 4096, SHA256: "AB"})
	out := destfs.New(dir)
	p := filepath.Join(dir, "manifest.json")
	if err := m.ExportJSON(out, p); err != nil {
		t.Fatal(err)
	}
	if err := m.ExportJSON(out, p); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int
		Entries []Entry
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Entries) != 1 || doc.Entries[0].Status != PARTIAL {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp left")
	}
}

func TestBetter(t *testing.T) {
	ok := Entry{Status: OK}
	p1 := Entry{Status: PARTIAL, BadBytes: 4096}
	p2 := Entry{Status: PARTIAL, BadBytes: 8192}
	cases := []struct {
		old, new Entry
		want     bool
	}{
		{Entry{Status: FAILED}, p2, true}, {p2, p1, true}, {p1, p2, false}, {p1, ok, true},
		{ok, p1, false}, {ok, Entry{Status: FAILED}, false}, {p1, Entry{Status: STALLED}, false},
		{Entry{}, Entry{Status: FAILED}, true}, {Entry{Status: LOCKED}, Entry{Status: FAILED}, false},
	}
	for i, c := range cases {
		if got := Better(c.old, c.new); got != c.want {
			t.Errorf("case %d: Better(%v,%v)=%v", i, c.old.Status, c.new.Status, got)
		}
	}
}

var _ = time.Now

func TestStoreCorruptRebuilds(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Photos"), 0o755)
	os.WriteFile(filepath.Join(dir, "Photos", "a.jpg"), []byte("good bytes"), 0o644)
	os.WriteFile(filepath.Join(dir, "Photos", "b.jpg"), []byte("changed!!"), 0o644)
	s, m := open(t, filepath.Join(dir, "old.db"))
	m.Volume = drives.Identity{FSSerial: "S1"}
	m.SaveMeta()
	m.Put(Entry{Source: "a.jpg", Dest: "Photos/a.jpg", Status: OK, Bytes: 10, SHA256: sha("good bytes")})
	m.Put(Entry{Source: "b.jpg", Dest: "Photos/b.jpg", Status: OK, Bytes: 9, SHA256: sha("original!")})
	m.Put(Entry{Source: "c.jpg", Dest: "Photos/c.jpg", Status: OK, Bytes: 3, SHA256: sha("abc")})
	jsonPath := filepath.Join(dir, "manifest.json")
	m.ExportJSON(destfs.New(dir), jsonPath)
	s.Close()

	s2, fresh := open(t, filepath.Join(dir, "state.db"))
	defer s2.Close()
	n, err := Rebuild(fresh, jsonPath, dir)
	if err != nil || n != 1 {
		t.Fatalf("rebuilt %d, %v", n, err)
	}
	if fresh.Volume.FSSerial != "S1" {
		t.Fatal("volume identity not restored")
	}
	if _, ok, _ := fresh.Get("a.jpg"); !ok {
		t.Fatal("verified file not restored")
	}
	if _, ok, _ := fresh.Get("b.jpg"); ok {
		t.Fatal("file whose bytes changed was trusted")
	}
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}
