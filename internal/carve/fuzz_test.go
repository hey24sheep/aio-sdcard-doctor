package carve

import (
	"bytes"
	"io"
	"testing"
)

// boundReader fails the test on any read outside [lo, hi) and counts bytes read.
type boundReader struct {
	t      *testing.T
	r      io.ReaderAt
	lo, hi int64
	n      int64
}

func (b *boundReader) ReadAt(p []byte, off int64) (int, error) {
	if off < b.lo || off+int64(len(p)) > b.hi {
		b.t.Fatalf("read [%d,%d) outside [%d,%d)", off, off+int64(len(p)), b.lo, b.hi)
	}
	b.n += int64(len(p))
	return b.r.ReadAt(p, off)
}

// FuzzSize runs every format's Size on arbitrary input: no panic, size within the limit, reads only
// inside the limit and bounded in total. (One target covers every format; corpus = every generator.)
func FuzzSize(f *testing.F) {
	for _, g := range generators {
		f.Add(g.gen(f))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		limit := int64(len(data))
		for _, fm := range Registry {
			br := &boundReader{t: t, r: bytes.NewReader(data), lo: 0, hi: limit}
			n, _, err := fm.Size(br, 0, limit)
			if err == nil && (n < 0 || n > limit) {
				t.Fatalf("%s: size %d outside 0..%d", fm.Name, n, limit)
			}
			if br.n > 8*limit+1<<20 {
				t.Fatalf("%s: read %d bytes of a %d-byte input", fm.Name, br.n, limit)
			}
		}
	})
}
