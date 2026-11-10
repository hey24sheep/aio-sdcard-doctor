package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

func TestWorkerDeletedAndLive(t *testing.T) {
	img := testutil.FAT32Image(t, testutil.Card{
		"DCIM/KEEP.JPG": {Data: testutil.JPEG(70_000, "keep")},
		"GONE.JPG":      {Data: testutil.JPEG(9_000, "gone")},
	}, 64<<20)
	testutil.DeleteFATFile(t, img, "GONE    JPG")
	c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
	var del []source.Deleted
	if err := c.Deleted(context.Background(), func(d source.Deleted) error { del = append(del, d); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(del) != 1 || del[0].Path != "_ONE.JPG" || del[0].State != "recoverable" || del[0].Size != 9000 {
		t.Fatalf("deleted %+v", del)
	}
	runs := map[string]int64{}
	if err := c.LiveExtents(context.Background(), func(p string, off, n int64) error { runs[p] += n; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs["DCIM/KEEP.JPG"] != 70_000 {
		t.Fatalf("live %v", runs)
	}
}

func TestWorkerDeletedUnsupported(t *testing.T) {
	c := spawn(t, Spec{From: t.TempDir()}, 5*time.Second) // an OS folder has no deleted records
	err := c.Deleted(context.Background(), func(source.Deleted) error { return nil })
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
}

type manyDeleted struct{ source.Source }

func (manyDeleted) Deleted(ctx context.Context, emit func(source.Deleted) error) error {
	for i := 0; i < 25; i++ {
		if err := emit(source.Deleted{Path: fmt.Sprint(i)}); err != nil {
			return err
		}
	}
	return nil
}

// TestDeletedPaging: pages hand out every record once, in order, and a fresh cursor (a respawned
// worker) recollects.
func TestDeletedPaging(t *testing.T) {
	old := pageSize
	pageSize = 10
	defer func() { pageSize = old }()
	s := &Service{src: manyDeleted{}}
	var got []string
	for cur := 0; ; {
		var p DeletedPage
		if err := s.Deleted(PageArgs{Cursor: cur}, &p); err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Items {
			got = append(got, d.Path)
		}
		if p.Done {
			break
		}
		cur = p.Next
		if cur == 20 {
			s.deleted = nil // worker restarted mid-way
		}
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12",
		"13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24"}) {
		t.Fatal(got)
	}
}

// TestWorkerValidates: validation of a found file decodes untrusted data, so it runs in the worker.
func TestWorkerValidates(t *testing.T) {
	img := testutil.FAT32Image(t, testutil.Card{"A.BIN": {Data: make([]byte, 4096)}}, 64<<20)
	c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
	ok, why, err := c.Validate("jpg", 0, [][2]int64{{0, 4096}}, 4096) // a boot sector is no JPEG
	if err != nil || ok || why == "" {
		t.Fatalf("ok %v why %q err %v", ok, why, err)
	}
	ok, _, err = c.Validate("bin", 0, [][2]int64{{0, 4096}}, 4096) // no check for unknown types
	if err != nil || !ok {
		t.Fatalf("bin: %v %v", ok, err)
	}
}

// TestDeletedCapped (review I6): a listing that passes the cap ends with what was found and says
// so, instead of failing the whole recover.
func TestDeletedCapped(t *testing.T) {
	old := maxListed
	maxListed = 10
	defer func() { maxListed = old }()
	s := &Service{src: manyDeleted{}}
	var p DeletedPage
	if err := s.Deleted(PageArgs{}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 10 || !p.Done || !p.Capped {
		t.Fatalf("%d items done %v capped %v", len(p.Items), p.Done, p.Capped)
	}
}
