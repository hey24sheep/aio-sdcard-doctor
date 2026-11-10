// Package salvage copies a file off a damaged card, keeping every readable byte. It is an exact
// port of the reference script's Copy-Salvage: 1 MiB chunks with retries, then 4 KiB blocks,
// unreadable blocks zero-filled, and a "dead zone" mode so a large damaged area can't stall the
// copy for hours. Reading and writing overlap (double buffering), but the card still sees one
// sequential reader.
package salvage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync/atomic"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

const (
	ChunkSize   = 1 << 20 // 1 MiB
	BlockSize   = 4 << 10 // 4 KiB
	ChunkTries  = 3       // retries after the first attempt (4 reads), like -Retries 3
	DeadZoneRun = 16      // consecutive failed blocks that start a dead zone / probe interval
)

// RetrySleep is the pause between retries (300 ms like the script; tests set 0).
var RetrySleep = 300 * time.Millisecond

// Result of one copy.
type Result struct {
	Bad     int64    // bytes zero-filled because they could not be read
	Sum     [32]byte // SHA-256 of what was written
	Retried int      // failed read attempts (feeds the card health monitor)
}

// Copier holds two reusable 1 MiB buffers. Not safe for concurrent use.
type Copier struct {
	bufs [2][]byte
	h    hash.Hash
}

// NewCopier allocates the buffers once for the whole run.
func NewCopier() *Copier {
	return &Copier{bufs: [2][]byte{make([]byte, ChunkSize), make([]byte, ChunkSize)}, h: sha256.New()}
}

// fatal errors come from the reader process and must not be salvaged as bad sectors.
func fatal(err error) bool {
	return errors.Is(err, source.ErrStalled) || errors.Is(err, source.ErrWorkerCrashed) ||
		errors.Is(err, source.ErrTooManyRestarts)
}

type reader struct {
	ctx     context.Context
	src     io.ReaderAt
	retried int
	fatal   error
}

// readAt reads exactly len(p) bytes or up to EOF (Read-At) with `tries` retries
// (Read-WithRetry). It returns -1 when every attempt failed.
func (r *reader) readAt(p []byte, off int64, tries int) int {
	for a := 0; a <= tries; a++ {
		if r.ctx.Err() != nil || r.fatal != nil {
			return -1
		}
		n, err := r.src.ReadAt(p, off)
		if err == nil || errors.Is(err, io.EOF) {
			return n
		}
		if fatal(err) {
			r.fatal = err
			return -1
		}
		r.retried++
		if a < tries && RetrySleep > 0 {
			t := time.NewTimer(RetrySleep)
			select {
			case <-r.ctx.Done():
			case <-t.C:
			}
			t.Stop()
		}
	}
	return -1
}

func clear0(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

type chunk struct {
	buf []byte
	n   int
}

// Copy copies size bytes of src to dst. Unreadable 4 KiB blocks become zeros and are counted in
// Result.Bad. Reader-process failures (stalled, crashed, too many restarts), ctx cancellation
// and destination write errors stop the copy and are returned.
func (c *Copier) Copy(ctx context.Context, src io.ReaderAt, size int64, dst io.Writer) (Result, error) {
	var res Result
	c.h.Reset()
	work := make(chan chunk)
	free := make(chan []byte, 2)
	free <- c.bufs[0]
	free <- c.bufs[1]
	var werrFlag atomic.Bool
	done := make(chan error, 1)
	go func() { // the single writer: write + hash, hand the buffer back
		var werr error
		for ch := range work {
			if werr == nil {
				if _, err := dst.Write(ch.buf[:ch.n]); err != nil {
					werr = err
					werrFlag.Store(true)
				} else {
					c.h.Write(ch.buf[:ch.n])
				}
			}
			free <- ch.buf
		}
		done <- werr
	}()
	rd := &reader{ctx: ctx, src: src}
	err := c.readLoop(ctx, rd, size, &res, work, free, &werrFlag)
	close(work)
	werr := <-done
	res.Retried = rd.retried
	switch {
	case werr != nil:
		return res, fmt.Errorf("writing the copy: %w", werr)
	case err != nil:
		return res, err
	}
	copy(res.Sum[:], c.h.Sum(nil))
	return res, nil
}

func (c *Copier) readLoop(ctx context.Context, rd *reader, size int64, res *Result, work chan<- chunk, free <-chan []byte, werr *atomic.Bool) error {
	deadZone := false
	for pos := int64(0); pos < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if werr.Load() {
			return nil // the writer reports its error
		}
		buf := <-free
		want := int(min(ChunkSize, size-pos))
		b := buf[:want]
		tries := ChunkTries
		if deadZone {
			tries = 0
		}
		n := rd.readAt(b, pos, tries)
		switch {
		case rd.fatal != nil:
			return rd.fatal
		case ctx.Err() != nil:
			return ctx.Err()
		case n >= 0:
			deadZone = false
			if n < want { // source ended early (truncated file): pad
				clear0(b[n:])
				res.Bad += int64(want - n)
			}
		case deadZone: // inside a large unreadable area: zero the chunk, no per-block retries
			clear0(b)
			res.Bad += int64(want)
		default: // block by block, keeping everything readable
			failRun := 0
			for o := 0; o < want; o += BlockSize {
				if err := ctx.Err(); err != nil {
					return err
				}
				cnt := min(BlockSize, want-o)
				r := -1
				switch {
				case failRun < DeadZoneRun:
					r = rd.readAt(b[o:o+cnt], pos+int64(o), 1)
				case failRun%DeadZoneRun == 0:
					r = rd.readAt(b[o:o+cnt], pos+int64(o), 0)
				}
				if rd.fatal != nil {
					return rd.fatal
				}
				if r < cnt {
					got := max(r, 0)
					clear0(b[o+got : o+cnt])
					res.Bad += int64(cnt - got)
					failRun++
				} else {
					failRun = 0
				}
			}
			if failRun >= DeadZoneRun {
				deadZone = true
			}
		}
		work <- chunk{buf, want}
		pos += int64(want)
	}
	return nil
}
