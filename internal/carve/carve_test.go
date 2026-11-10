package carve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

func format(t testing.TB, name string) Format {
	t.Helper()
	for _, f := range Registry {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("format %q not in Registry", name)
	return Format{}
}

func noise(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// TestFormatSizes: every format → exact size with trailing garbage, Complete=false when truncated.
func TestFormatSizes(t *testing.T) {
	for _, g := range generators {
		t.Run(g.format+"/"+g.ext, func(t *testing.T) {
			data := g.gen(t)
			f := format(t, g.format)
			buf := append(append([]byte{}, data...), noise(8192, 1)...)
			n, complete, err := f.Size(bytes.NewReader(buf), 0, int64(len(buf)))
			if err != nil || !complete || n != int64(len(data)) {
				t.Fatalf("size = %d complete=%v err=%v, want %d complete", n, complete, err, len(data))
			}
			// the same file at a non-zero offset
			pre := noise(1000, 2)
			buf2 := append(append(pre, data...), noise(100, 3)...)
			if n, ok, err := f.Size(bytes.NewReader(buf2), 1000, int64(len(buf2)-1000)); err != nil || !ok || n != int64(len(data)) {
				t.Fatalf("at offset 1000: %d %v %v", n, ok, err)
			}
			// truncated: the structure runs past the end
			cut := int64(len(data) * 6 / 10)
			n, complete, err = f.Size(bytes.NewReader(data[:cut]), 0, cut)
			if err != nil || complete || n > cut {
				t.Fatalf("truncated: size %d complete=%v err=%v (limit %d)", n, complete, err, cut)
			}
			// identified with the right extension
			var hits []Hit
			Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), Options{Align: 512}, 0,
				func(h Hit) error { hits = append(hits, h); return nil }, nil)
			if len(hits) == 0 || hits[0].Offset != 0 || hits[0].Ext != g.ext || hits[0].Size != int64(len(data)) || !hits[0].Complete {
				t.Fatalf("scan hits %+v, want %s at 0 size %d", hits, g.ext, len(data))
			}
			want, _ := profile.ByExt(g.ext)
			if hits[0].Cat == "" || hits[0].Cat != want && g.ext != "sqlite" {
				t.Fatalf("category %q for .%s (want %q)", hits[0].Cat, g.ext, want)
			}
		})
	}
}

type placed struct {
	off  int64
	data []byte
	ext  string
}

func synthetic(t testing.TB, size int, aligned, unaligned []int) ([]byte, []placed) {
	buf := noise(size, 42)
	var ps []placed
	gens := generators
	put := func(off int, i int) {
		g := gens[i%len(gens)]
		d := g.gen(t)
		copy(buf[off:], d)
		ps = append(ps, placed{int64(off), d, g.ext})
	}
	for i, off := range aligned {
		put(off, i)
	}
	for i, off := range unaligned {
		put(off, i+7)
	}
	return buf, ps
}

func scanAll(t testing.TB, buf []byte, opt Options) []Hit {
	var hits []Hit
	err := Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), opt, 0, func(h Hit) error {
		hits = append(hits, h)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func findHit(hits []Hit, p placed) bool {
	for _, h := range hits {
		if h.Offset == p.off && h.Size == int64(len(p.data)) && h.Ext == p.ext && h.Complete {
			return true
		}
	}
	return false
}

// TestScanSyntheticImage: files at sector and non-sector offsets in noise → aligned mode finds the
// aligned ones, thorough mode finds all of them; nothing else is reported as complete.
func TestScanSyntheticImage(t *testing.T) {
	var aligned, unaligned []int
	for i := range generators {
		aligned = append(aligned, (i*2+1)*64*1024)
		unaligned = append(unaligned, (i*2+2)*64*1024+100)
	}
	buf, ps := synthetic(t, (len(generators)*2+4)*64*1024, aligned, unaligned)
	for _, mode := range []struct {
		align int
		want  int
	}{{512, len(aligned)}, {1, len(aligned) + len(unaligned)}} {
		hits := scanAll(t, buf, Options{Align: mode.align})
		found := 0
		for i, p := range ps {
			if findHit(hits, p) {
				found++
			} else if i < len(aligned) || mode.align == 1 {
				t.Errorf("align %d: %s at %d (size %d) not found", mode.align, p.ext, p.off, len(p.data))
			}
		}
		extra := 0
		for _, h := range hits {
			ok := false
			for _, p := range ps {
				ok = ok || h.Offset == p.off
			}
			if !ok && h.Complete {
				extra++
				t.Logf("align %d: unexpected %+v", mode.align, h)
			}
		}
		if found != mode.want || extra > 0 {
			t.Fatalf("align %d: found %d of %d, %d unexpected complete hits", mode.align, found, mode.want, extra)
		}
	}
}

func TestScanSkipsRegions(t *testing.T) {
	buf, ps := synthetic(t, 1<<20, []int{8192, 512 * 1024}, nil)
	skip := [][2]int64{{ps[0].off, ps[0].off + int64(len(ps[0].data))}}
	hits := scanAll(t, buf, Options{Align: 512, Skip: skip})
	for _, h := range hits {
		if h.Offset >= skip[0][0] && h.Offset < skip[0][1] {
			t.Fatalf("hit inside a skipped region: %+v", h)
		}
	}
	if !findHit(hits, ps[1]) {
		t.Fatal("file outside the skipped region not found")
	}
}

func TestScanFormatsFilter(t *testing.T) {
	buf, ps := synthetic(t, 1<<20, []int{8192, 512 * 1024}, nil) // jpeg, png
	hits := scanAll(t, buf, Options{Align: 512, Formats: []string{"png"}})
	if findHit(hits, ps[0]) || !findHit(hits, ps[1]) {
		t.Fatalf("formats filter: %+v", hits)
	}
}

func TestScanResumeFromCheckpoint(t *testing.T) {
	old := checkpointEvery
	checkpointEvery = 256 * 1024
	defer func() { checkpointEvery = old }()
	var offs []int
	for i := 0; i < 24; i++ {
		offs = append(offs, (i+1)*128*1024)
	}
	buf, _ := synthetic(t, 26*128*1024, offs, nil)
	full := scanAll(t, buf, Options{Align: 512})

	errStop := errors.New("stop")
	var first []Hit
	var cp int64
	err := Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), Options{Align: 512}, 0, func(h Hit) error {
		if len(first) == 10 {
			return errStop
		}
		first = append(first, h)
		return nil
	}, func(pos int64) error { cp = pos; return nil })
	if !errors.Is(err, errStop) || cp == 0 {
		t.Fatalf("err %v checkpoint %d", err, cp)
	}
	var got []Hit
	for _, h := range first {
		if h.Offset < cp {
			got = append(got, h)
		}
	}
	Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), Options{Align: 512}, cp,
		func(h Hit) error { got = append(got, h); return nil }, nil)
	if fmt.Sprint(got) != fmt.Sprint(full) {
		t.Fatalf("resumed hits differ:\n%v\n%v", got, full)
	}
}

func TestScanGarbageNoCrash(t *testing.T) {
	size := 64 << 20
	if testing.Short() {
		size = 4 << 20
	}
	buf := noise(size, 9)
	for _, align := range []int{512, 1} {
		if align == 1 {
			buf = buf[:4<<20]
		}
		for _, h := range scanAll(t, buf, Options{Align: align}) {
			if h.Size <= 0 || h.Offset+h.Size > int64(len(buf)) {
				t.Fatalf("bad hit %+v", h)
			}
		}
	}
}

func TestScanCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Scan(ctx, bytes.NewReader(noise(4<<20, 1)), 4<<20, Options{}, 0, func(Hit) error { return nil }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// TestScanReadErrorsSkipped: an unreadable window does not stop the scan.
func TestScanReadErrorsSkipped(t *testing.T) {
	buf, ps := synthetic(t, 4<<20, []int{3 << 20}, nil)
	r := &badReader{r: bytes.NewReader(buf), from: 1 << 20, to: 2 << 20}
	var hits []Hit
	if err := Scan(context.Background(), r, int64(len(buf)), Options{}, 0, func(h Hit) error { hits = append(hits, h); return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if !findHit(hits, ps[0]) {
		t.Fatal("file after the bad area not found")
	}
}

type badReader struct {
	r        *bytes.Reader
	from, to int64
}

func (b *badReader) ReadAt(p []byte, off int64) (int, error) {
	if off < b.to && off+int64(len(p)) > b.from {
		return 0, errors.New("I/O error")
	}
	return b.r.ReadAt(p, off)
}

func TestUnsupportedListed(t *testing.T) {
	for _, k := range []string{"crypt14", "mxf", "txt"} {
		if Unsupported[k] == "" {
			t.Errorf("%s has no reason", k)
		}
	}
}
