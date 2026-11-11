package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// rawCard writes an unreadable card (no file system) of size bytes with a photo at off.
func rawCard(t *testing.T, size, off int64, photo []byte) string {
	b := make([]byte, size)
	copy(b[off:], photo)
	p := filepath.Join(t.TempDir(), "raw.img")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Adversarial pass D2: two unreadable cards of the same size shared one identity (size only), so
// the second card's deep scan was skipped and the first card's photo counted as already copied.
func TestTwoRawCardsSameSizeAreDifferentCards(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	a := rawCard(t, 8<<20, 1<<20, realJPEG(t, 120, 90, 3))
	run(t, &Recover{From: a, Dest: dst, Deep: true})
	b := rawCard(t, 8<<20, 3<<20, realJPEG(t, 100, 70, 9))
	r := &Recover{From: b, Dest: dst, Deep: true, StallTimeout: 5e9}
	defer r.Close()
	p, err := r.Plan(context.Background(), env())
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range p.Issues {
		if is.Kind == "dest-other-card" {
			return
		}
	}
	t.Fatalf("card B planned into card A's recovery folder: %+v", p.Issues)
}

// Adversarial pass D4: following the hint "run recover again with --select <file>" without
// --deep matched nothing and recovered nothing, clean, exit 0; and a list re-saved by Excel with
// ";" separators matched nothing either.
func TestSelectDeepPicksWithoutDeepFlag(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	photo := realJPEG(t, 120, 90, 5)
	card := rawCard(t, 8<<20, 2<<20, photo)
	run(t, &Recover{From: card, Dest: dst, Deep: true, List: true}) // the list: nothing written
	ids := []string{"C:" + fmt.Sprint(2<<20)}
	_, res := run(t, &Recover{From: card, Dest: dst, Select: ids})
	if res.Counters["CARVED"] != 1 {
		t.Fatalf("picked deep-scan find not recovered: %v", res.Counters)
	}
	r := &Recover{From: card, Dest: filepath.Join(t.TempDir(), "out2"), Select: []string{"C:999999999"}, StallTimeout: 5e9}
	defer r.Close()
	p, err := r.Plan(context.Background(), env())
	if err == nil {
		_, err = r.Run(context.Background(), env(), p)
	}
	if err == nil {
		t.Fatal("picks that match nothing recovered nothing, without a word")
	}
	sel := filepath.Join(t.TempDir(), "picks.csv")
	os.WriteFile(sel, []byte("State;Name;Path;Size;Modified;Comment;ID\nExcellent;a.jpg;a.jpg;1;x;;D:a.jpg\n"), 0o644)
	if got, _ := ReadSelection(sel); len(got) != 1 || got[0] != "D:a.jpg" {
		t.Fatalf("';' list read as %q", got)
	}
}
