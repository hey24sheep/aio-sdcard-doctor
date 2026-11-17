package scan

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// damaged is a card whose folder listings come from list (a damaged FAT directory).
type damaged struct {
	fstest.MapFS
	list func(name string) ([]fs.DirEntry, error)
}

func (d damaged) ReadDir(name string) ([]fs.DirEntry, error) { return d.list(name) }
func (d damaged) OpenAt(string) (io.ReaderAt, int64, io.Closer, error) {
	return nil, 0, nil, fs.ErrNotExist
}
func (d damaged) Kind() string              { return "fat32" }
func (d damaged) Identity() drives.Identity { return drives.Identity{} }

// I8: a damaged folder listing one name twice gave two items with one path; the second was
// reported "already copied" without being read.
func TestSameNameTwiceIsListedOnce(t *testing.T) {
	m := fstest.MapFS{"DCIM/IMG_0001.JPG": {Data: []byte("one")}, "DCIM/IMG_0002.JPG": {Data: []byte("two")}}
	src := damaged{MapFS: m, list: func(name string) ([]fs.DirEntry, error) {
		es, err := m.ReadDir(name)
		if name == "DCIM" && len(es) > 0 {
			es = append(es, es[0])
		}
		return es, err
	}}
	inv, err := Scan(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, it := range inv.Items {
		if seen[it.Rel] {
			t.Fatalf("%s listed twice", it.Rel)
		}
		seen[it.Rel] = true
	}
	if len(inv.Items) != 2 || len(inv.Unreadable) != 1 || !strings.Contains(inv.Unreadable[0], "appears twice") {
		t.Fatalf("items %d, unreadable %q", len(inv.Items), inv.Unreadable)
	}
}

// I9: a folder entry pointing back at its parent made an endless tree; the scan never ended.
func TestFolderLoopStopsAtDepthCap(t *testing.T) {
	m := fstest.MapFS{"loop/a.jpg": {Data: []byte("a")}}
	src := damaged{MapFS: m, list: func(string) ([]fs.DirEntry, error) { return m.ReadDir(".") }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inv, err := Scan(ctx, src, nil)
	if err != nil {
		t.Fatalf("scan didn't end: %v", err)
	}
	if len(inv.Unlistable) != 1 || !strings.Contains(inv.Unlistable[0], "levels") {
		t.Fatalf("unlistable %q", inv.Unlistable)
	}
}

// OR1: one unreadable sector in a folder hid every file of it. The reader returns the entries
// it could read plus an error: those files are listed and the folder is still reported.
func TestPartlyListedFolderKeepsItsReadableFiles(t *testing.T) {
	m := fstest.MapFS{"DCIM/a.jpg": {Data: []byte("a")}, "DCIM/b.jpg": {Data: []byte("b")}}
	src := damaged{MapFS: m, list: func(name string) ([]fs.DirEntry, error) {
		es, err := m.ReadDir(name)
		if name == "DCIM" {
			return es, errors.New("unreadable sector in the folder")
		}
		return es, err
	}}
	inv, err := Scan(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Items) != 2 {
		t.Fatalf("readable files of a damaged folder dropped: %d items", len(inv.Items))
	}
	if len(inv.Unlistable) != 1 || !strings.HasPrefix(inv.Unlistable[0], "DCIM: ") {
		t.Fatalf("unlistable %q", inv.Unlistable)
	}
}
