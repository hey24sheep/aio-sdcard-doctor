package report

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
)

func TestStampMillis(t *testing.T) {
	ts := time.Date(2026, 11, 9, 21, 36, 18, 445_900_000, time.Local)
	if got := Stamp(ts); got != "20261109_213618_445" {
		t.Fatalf("got %s", got)
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "copy_x.csv")
	if got := UniquePath(p); got != p {
		t.Fatalf("free path changed: %s", got)
	}
	os.WriteFile(p, nil, 0o644)
	os.WriteFile(filepath.Join(dir, "copy_x_2.csv"), nil, 0o644)
	if got := UniquePath(p); got != filepath.Join(dir, "copy_x_3.csv") {
		t.Fatalf("got %s", got)
	}
}

func entries() []manifest.Entry {
	return []manifest.Entry{
		{Status: manifest.OK, Category: "Photos", Source: "DCIM/a.jpg", Dest: "Photos/DCIM/a.jpg", Bytes: 10, Header: "ok", SHA256: "AA",
			MTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)},
		{Status: manifest.PARTIAL, Category: "Photos", Source: "DCIM/b,\"q\".jpg", Dest: "Photos/DCIM/b__PARTIAL.jpg", BadBytes: 4096, Header: "mismatch"},
		{Status: manifest.DELETED_UNRECOVERABLE, Source: "DCIM/gone.jpg", Error: "overwritten"},
		{Status: manifest.EXCLUDED, Source: ".thumbnails/t.jpg", Error: "thumbnails"},
	}
}

func TestCSVHeaderAndBOM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "copy.csv")
	es := entries()
	i := 0
	err := WriteCSV(destfs.New(dir), p, func(fn func(manifest.Entry) error) error {
		for ; i < len(es); i++ {
			if err := fn(es[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		t.Fatal("no BOM")
	}
	rows, err := csv.NewReader(bytes.NewReader(b[3:])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := "Status,Category,Source,Dest,Bytes,BadBytes,Header,SHA256,Error,MTime"
	if strings.Join(rows[0], ",") != want || len(rows) != 5 {
		t.Fatalf("header %v rows %d", rows[0], len(rows))
	}
	if rows[2][2] != "DCIM/b,\"q\".jpg" || rows[1][9] != "2026-01-02 03:04:05" {
		t.Fatalf("row %v / %v", rows[2], rows[1])
	}
}

func TestSummaryListsUnrecoverableWithPaths(t *testing.T) {
	s := Summary{Title: "Copy", Source: "E:", Dest: `D:\r`, Took: 3 * time.Minute, Kind: "folder",
		Profiles: []string{"Android"}, Excluded: map[string]int{"thumbnails": 1}, CSV: "copy.csv",
		Issues: []string{"card removed — reinserted"}, Backup: "Quick"}
	for _, e := range entries() {
		s.Add(e)
	}
	text := s.String()
	for _, want := range []string{"OK", "PARTIAL", "[mismatch] DCIM/b", "DCIM/gone.jpg — overwritten",
		"thumbnails", "Android", "card removed", "copy.csv", "PARTIAL = ", "Backup choice: Quick"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
}

// Adversarial pass (minor): a card file named "=HYPERLINK(...)" became a live formula when the
// report was opened in a spreadsheet. Such cells start with ' — and read back unchanged.
func TestCellNeutralizesFormulas(t *testing.T) {
	for _, s := range []string{"=HYPERLINK(\"x\")", "+1", "-cmd", "@SUM(A1)"} {
		if c := Cell(s); c[0] != '\'' || FromCell(c) != s {
			t.Errorf("%q → %q → %q", s, c, FromCell(c))
		}
	}
	if Cell("DCIM/a.jpg") != "DCIM/a.jpg" {
		t.Error("an ordinary name changed")
	}
}
