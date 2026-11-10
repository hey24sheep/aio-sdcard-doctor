// Package faultreader is a test io.ReaderAt over a byte slice that fails reads touching bad
// ranges, to exercise salvage, imaging and the worker without a damaged card.
package faultreader

import (
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// ErrBadSector is the default error for reads that touch a bad range.
var ErrBadSector = errors.New("faultreader: unreadable sector")

// Range is a bad area [Off, Off+Len).
type Range struct{ Off, Len int64 }

// Options shape the reader's behaviour.
type Options struct {
	Delay     time.Duration                    // every read sleeps this long first
	FailDelay time.Duration                    // failing reads sleep this long (a dying card)
	Sleep     func(time.Duration)              // clock seam for Delay/FailDelay (default time.Sleep)
	ShortAt   int64                            // >0: the data appears to end here (truncated file)
	Err       error                            // error for bad reads (default ErrBadSector)
	Block     <-chan struct{}                  // reads block until this is closed
	OnRead    func(off int64, n int) (e error) // optional hook; a non-nil error fails the read
}

// Reader is the fault-injecting io.ReaderAt.
type Reader struct {
	data  []byte
	bad   []Range
	opt   Options
	calls atomic.Int64
}

// New returns a Reader over data.
func New(data []byte, bad []Range, opt Options) *Reader {
	if opt.Err == nil {
		opt.Err = ErrBadSector
	}
	if opt.Sleep == nil {
		opt.Sleep = time.Sleep
	}
	return &Reader{data: data, bad: bad, opt: opt}
}

// Calls is the number of ReadAt calls so far.
func (r *Reader) Calls() int64 { return r.calls.Load() }

// Size is the readable size.
func (r *Reader) Size() int64 {
	if r.opt.ShortAt > 0 {
		return r.opt.ShortAt
	}
	return int64(len(r.data))
}

// SetBad replaces the bad ranges (a card getting worse, or a retry pass succeeding).
func (r *Reader) SetBad(bad []Range) { r.bad = bad }

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.calls.Add(1)
	if r.opt.Block != nil {
		<-r.opt.Block
	}
	if r.opt.Delay > 0 {
		r.opt.Sleep(r.opt.Delay)
	}
	if r.opt.OnRead != nil {
		if err := r.opt.OnRead(off, len(p)); err != nil {
			return 0, err
		}
	}
	end := off + int64(len(p))
	for _, b := range r.bad {
		if off < b.Off+b.Len && b.Off < end {
			if r.opt.FailDelay > 0 {
				r.opt.Sleep(r.opt.FailDelay)
			}
			return 0, r.opt.Err
		}
	}
	size := r.Size()
	if off >= size {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:size])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
