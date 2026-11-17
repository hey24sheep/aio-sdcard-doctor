package job

import (
	"path/filepath"
	"testing"
)

// SR10: a deep scan stored each find in its own fsync'd transaction (50,000 finds, 50,000
// syncs on an SD card destination). A batch writes them together, at the scan's checkpoint.
func TestBatchWritesTogetherOnFlush(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := NewBatch(s)
	for i, k := range []string{"a", "b", "c"} {
		if err := BatchPut(b, "hits", k, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := Get[int](s, "hits", "a"); ok {
		t.Fatal("written before Flush")
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	n := 0
	ForEach(s, "hits", func(string, int) error { n++; return nil })
	if n != 3 {
		t.Fatalf("%d of 3 written", n)
	}
	if err := b.Flush(); err != nil { // nothing left: no error, nothing written twice
		t.Fatal(err)
	}
}
