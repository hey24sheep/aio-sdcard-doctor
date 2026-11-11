package carve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// checkpointEvery is how often Scan reports a resume position (a variable for tests).
var checkpointEvery = 64 * mib

const (
	window = 1 << 20
	piece  = 64 << 10 // re-read size inside a window with a bad area
)

type lookup struct {
	off int
	idx [256][]int // first byte at off → candidate formats
}

// Scan reads r sequentially in 1 MiB windows from `from` and reports every file whose signature
// is at an aligned offset. Complete hits are skipped over; skip regions are never looked into.
// checkpoint(pos) means every hit before pos has been emitted: Scan(…, from=pos, …) resumes.
// Unreadable windows are passed over.
func Scan(ctx context.Context, r io.ReaderAt, size int64, opt Options, from int64,
	emit func(Hit) error, checkpoint func(pos int64) error) error {
	r = ctxReader{ctx: ctx, r: r} // a stuck walk notices a cancel at its next read
	align := int64(opt.Align)
	if align <= 0 {
		align = 512
	}
	up := func(p int64) int64 { return (p + align - 1) / align * align }

	var fs []Format
	want := map[string]bool{}
	for _, n := range opt.Formats {
		want[n] = true
	}
	for _, f := range Registry {
		if len(want) == 0 || want[f.Name] {
			fs = append(fs, f)
		}
	}
	var tables []*lookup
	span := 0
	for i, f := range fs {
		for _, mg := range f.Magics {
			var t *lookup
			for _, x := range tables {
				if x.off == mg.Off {
					t = x
				}
			}
			if t == nil {
				t = &lookup{off: mg.Off}
				tables = append(tables, t)
			}
			l := t.idx[mg.Bytes[0]]
			if len(l) == 0 || l[len(l)-1] != i {
				t.idx[mg.Bytes[0]] = append(l, i)
			}
			span = max(span, mg.Off+len(mg.Bytes))
		}
	}
	skip := append([][2]int64(nil), opt.Skip...)
	sort.Slice(skip, func(i, j int) bool { return skip[i][0] < skip[j][0] })

	buf := make([]byte, window)
	var bufStart, bufLen int64 = -1, 0
	p := up(from)
	nextCP := (p/checkpointEvery + 1) * checkpointEvery
	tried := make([]bool, len(fs))
	suppress := make([]int64, len(fs)) // end of each format's last find: no nested finds of itself
	index := map[string]int{}
	for i, f := range fs {
		index[f.Name] = i
	}
	for p < size {
		if p >= nextCP {
			if checkpoint != nil {
				if err := checkpoint(p); err != nil {
					return err
				}
			}
			nextCP = (p/checkpointEvery + 1) * checkpointEvery
		}
		for len(skip) > 0 && skip[0][1] <= p {
			skip = skip[1:]
		}
		if len(skip) > 0 && skip[0][0] <= p {
			p = up(skip[0][1])
			continue
		}
		if bufStart < 0 || p < bufStart || p >= bufStart+bufLen || p+int64(span) > bufStart+bufLen && bufStart+bufLen < size {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(int64(window), size-p)
			got, err := r.ReadAt(buf[:n], p)
			if int64(got) < n && err != nil && !errors.Is(err, io.EOF) {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				// a bad area: read the rest in small pieces so one bad sector costs only its piece;
				// unreadable pieces are zero-filled (nothing matches zeros) and reported
				for q := int64(got); q < n; q += piece {
					k := min(piece, n-q)
					if m, err := r.ReadAt(buf[q:q+k], p+q); int64(m) < k && err != nil && !errors.Is(err, io.EOF) {
						if cerr := ctx.Err(); cerr != nil {
							return cerr
						}
						clear(buf[q+int64(m) : q+k])
						if opt.Unreadable != nil {
							opt.Unreadable(p+q+int64(m), k-int64(m))
						}
					}
				}
				got = int(n)
			}
			if got <= 0 {
				bufStart = -1
				p = up(p + n)
				continue
			}
			bufStart, bufLen = p, int64(got)
		}
		i := int(p - bufStart)
		for k := range tried {
			tried[k] = false
		}
		var hit, partial *Hit
		for _, t := range tables {
			if i+t.off >= int(bufLen) {
				continue
			}
			for _, fi := range t.idx[buf[i+t.off]] {
				if tried[fi] || suppress[fi] > p {
					continue
				}
				f := fs[fi]
				if !matches(f, buf[i:bufLen]) {
					continue
				}
				tried[fi] = true
				limit := size - p
				if f.MaxSize > 0 {
					limit = min(limit, f.MaxSize)
				}
				n, complete, err := safeSize(f, r, p, limit)
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				bad := errors.Is(err, ErrUnreadable)
				if err != nil && !bad || n <= 0 {
					continue
				}
				// a stream that runs into the end of what was searched (the card's end, its size
				// cap) may go on beyond it: not known to be whole
				complete = complete && !bad && !(f.Stream && n == limit)
				h := Hit{Offset: p, Size: n, Format: f.Name, Ext: f.Ext, Cat: f.Cat, Complete: complete, Unreadable: bad}
				if f.Sub != nil {
					if e := safeSub(f, r, p, n); e != "" {
						h.Ext = e
					}
				}
				if c, ok := profile.ByExt(h.Ext); ok {
					h.Cat = c
				}
				if complete {
					hit = &h
					break
				}
				if partial == nil {
					partial = &h
				}
			}
			if hit != nil {
				break
			}
		}
		switch {
		case hit != nil:
			if err := emit(*hit); err != nil {
				return err
			}
			if fs[index[hit.Format]].HeaderSized { // keep looking inside: the header may be stale
				suppress[index[hit.Format]] = p + hit.Size
				p += align
			} else {
				p = up(p + hit.Size)
			}
		case partial != nil:
			// one find per stream: the same format is not looked for again inside what it walked
			if err := emit(*partial); err != nil {
				return err
			}
			suppress[index[partial.Format]] = p + partial.Size
			p += align
		default:
			p += align
		}
	}
	if checkpoint != nil {
		return checkpoint(size)
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.ReaderAt
}

func (c ctxReader) ReadAt(p []byte, off int64) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.ReadAt(p, off)
}

func matches(f Format, b []byte) bool {
	for _, mg := range f.Magics {
		if mg.Off+len(mg.Bytes) <= len(b) && bytes.Equal(b[mg.Off:mg.Off+len(mg.Bytes)], mg.Bytes) {
			return true
		}
	}
	return false
}
