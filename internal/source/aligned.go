package source

import "io"

// Aligned makes any read work on a device that only accepts sector-aligned offsets and lengths:
// unaligned reads go through a bounce buffer covering the enclosing sectors. Reads past size
// return io.EOF with the bytes that exist.
type Aligned struct {
	r            io.ReaderAt
	sector, size int64
}

// NewAligned wraps r (sector size in bytes, device size in bytes).
func NewAligned(r io.ReaderAt, sector, size int64) *Aligned {
	if sector <= 0 {
		sector = 512
	}
	return &Aligned{r: r, sector: sector, size: size}
}

// ReadAt implements io.ReaderAt.
func (a *Aligned) ReadAt(p []byte, off int64) (int, error) {
	if off >= a.size {
		return 0, io.EOF
	}
	want := p
	short := false
	if off+int64(len(p)) > a.size {
		want, short = p[:a.size-off], true
	}
	var n int
	var err error
	if off%a.sector == 0 && int64(len(want))%a.sector == 0 {
		n, err = a.r.ReadAt(want, off)
	} else {
		start := off / a.sector * a.sector
		end := (off + int64(len(want)) + a.sector - 1) / a.sector * a.sector
		end = min(end, (a.size+a.sector-1)/a.sector*a.sector)
		buf := make([]byte, end-start)
		var m int
		m, err = a.r.ReadAt(buf, start)
		if err == io.EOF && int64(m) >= off-start+int64(len(want)) {
			err = nil
		}
		n = max(0, min(len(want), m-int(off-start)))
		copy(want, buf[off-start:int(off-start)+n])
	}
	if err == nil && short {
		err = io.EOF
	}
	return n, err
}
