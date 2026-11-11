package fix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"unsafe"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// RW is the raw card (or an image file) the health test writes and reads.
type RW interface {
	io.ReaderAt
	io.WriterAt
}

// Range is a byte range [Off, Off+N).
type Range struct{ Off, N int64 }

// Report is what the health test found.
type Report struct {
	Size         int64
	RealCapacity int64   // bytes that hold what was written
	Bad          []Range // areas that couldn't be written, read, or didn't read back
	Fake         bool    // the card claims more than it holds (writes wrap around)
}

// HealthOptions configures a run.
type HealthOptions struct {
	StorePath string             // progress file (in the user's config folder, never on the card)
	Replug    bool               // ask to unplug/replug between writing and reading (R7)
	Reopen    func() (RW, error) // reopen the card after the replug
	Progress  func(phase string, done, total int64)
	Restart   bool // --new-test: forget a stopped test (of this or another card) and start over
}

// Errors.
var (
	ErrDifferentCard = errors.New("this is not the card the test started on — put that card back, or start a new test with --new-test")
	ErrStopped       = errors.New("stopped — run the same command again to continue the test")
)

// healthCheckpoint: progress is saved this often (a variable for tests).
var healthCheckpoint int64 = 256 << 20

const (
	hChunk  = 1 << 20
	hBlock  = 4096
	hBucket = "health"
	hGone   = 4 // this many chunks failing in a row: the card is gone, stop (resume later)
)

type hstate struct {
	Seed    uint64
	Size    int64
	Phase   string // write, read, done
	Pos     int64
	Bad     []Range
	Aliased int64
	Blank   []Range // mismatched blocks that read back all 0x00 or all 0xFF (writes dropped)
}

// fillBlock writes block o's pattern: offset (8 bytes LE) | seed (8 bytes) | xorshift(offset, seed).
func fillBlock(b []byte, o int64, seed uint64) {
	var h [16]byte
	binary.LittleEndian.PutUint64(h[:], uint64(o))
	binary.LittleEndian.PutUint64(h[8:], seed)
	n := copy(b, h[:])
	x := uint64(o) ^ seed ^ 0x9E3779B97F4A7C15
	if x == 0 {
		x = 1
	}
	var w [8]byte
	for i := n; i < len(b); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(w[:], x)
		copy(b[i:], w[:])
	}
}

func fillChunk(b []byte, off int64, seed uint64) {
	for i := 0; i < len(b); i += hBlock {
		fillBlock(b[i:min(i+hBlock, len(b))], off+int64(i), seed)
	}
}

func addBad(bad []Range, off, n int64) []Range {
	if k := len(bad); k > 0 && bad[k-1].Off+bad[k-1].N == off {
		bad[k-1].N += n
		return bad
	}
	return append(bad, Range{off, n})
}

// union sorts ranges and merges overlapping or touching ones (an area can fail writing and
// reading both; it counts once).
func union(rs []Range) []Range {
	rs = append([]Range(nil), rs...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Off < rs[j].Off })
	var out []Range
	for _, r := range rs {
		if k := len(out); k > 0 && r.Off <= out[k-1].Off+out[k-1].N {
			out[k-1].N = max(out[k-1].N, r.Off+r.N-out[k-1].Off)
			continue
		}
		out = append(out, r)
	}
	return out
}

func inBad(bad []Range, off int64) bool {
	for _, r := range bad {
		if off >= r.Off && off < r.Off+r.N {
			return true
		}
	}
	return false
}

// HealthTest writes a unique pattern over the whole card, then reads it back. Bad areas and a
// card that wraps writes around (fake capacity) are reported. Progress is saved every
// healthCheckpoint bytes; a resumed run first checks the card still holds its pattern (R2).
func HealthTest(ctx context.Context, rw RW, size int64, env job.Env, opt HealthOptions) (Report, error) {
	st, err := job.OpenStore(opt.StorePath)
	if err != nil {
		return Report{}, err
	}
	defer st.Close()
	s, ok, _ := job.Get[hstate](st, hBucket, "state")
	if !ok || s.Size != size || s.Phase == "done" || opt.Restart {
		var seed [8]byte
		rand.Read(seed[:])
		s = hstate{Seed: binary.LittleEndian.Uint64(seed[:]) | 1, Size: size, Phase: "write"}
	} else if err := recheck(rw, s); err != nil {
		return Report{}, err
	}
	save := func() error { return job.Put(st, hBucket, "state", s) }
	if err := save(); err != nil {
		return Report{}, err
	}
	progress := func(phase string, done int64) {
		if opt.Progress != nil {
			opt.Progress(phase, done, size)
		}
		if env.Progress != nil {
			env.Progress(job.Progress{Phase: phase, Done: done, Total: size})
		}
	}
	buf := alignedBuf(hChunk)
	// stillThere tells a pulled card from a dead area: after hGone failing chunks in a row, the
	// last block that worked is read again. A card that still answers there is present, so the
	// failing area is bad (a failing card, or a fake refusing writes past its real size) and the
	// test goes on; one that doesn't answer is gone, and the run stops to resume later.
	probe, lastGood := alignedBuf(hBlock), int64(0)
	stillThere := func() bool {
		_, err := rw.ReadAt(probe, lastGood)
		return err == nil || errors.Is(err, io.EOF)
	}
	if s.Phase == "write" {
		bad, failing := append([]Range(nil), s.Bad...), 0
		for off := s.Pos; off < size; off += hChunk {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			n := min(hChunk, size-off)
			fillChunk(buf[:n], off, s.Seed)
			if _, err := rw.WriteAt(buf[:n], off); err != nil {
				if failing++; failing >= hGone {
					if !stillThere() {
						return Report{}, fmt.Errorf("the card stopped taking writes at %d MB (%w) — run the same command again to continue", off>>20, err)
					}
					failing = 0 // present: a dead area, not a pulled card
				}
				bad = addBad(bad, off, n)
			} else {
				failing, lastGood = 0, off
			}
			progress("Writing the test pattern", off+n)
			// never save past a streak of failed writes: if the card was pulled, a resume
			// rewrites those chunks instead of calling them bad
			if failing == 0 && ((off+n)%healthCheckpoint == 0 || off+n == size) {
				s.Pos, s.Bad = off+n, bad
				if err := save(); err != nil {
					return Report{}, err
				}
			}
		}
		s.Phase, s.Pos, s.Bad = "read", 0, bad
		if err := save(); err != nil {
			return Report{}, err
		}
		if opt.Replug {
			c := env.Prompt.Ask(ctx, job.Issue{Kind: "replug", Severity: job.Warn,
				Title:   "Unplug the card, then plug it back in",
				Detail:  "This makes the test read the card itself, not the computer's memory. Your computer will say the card needs formatting. Close that message — do not format. The test isn't finished.",
				Choices: []job.Choice{job.Continue, job.Stop}, Default: job.Continue})
			if c != job.Continue {
				return Report{}, ErrStopped
			}
			if opt.Reopen != nil {
				if rw, err = opt.Reopen(); err != nil {
					return Report{}, err
				}
			}
			if err := recheck(rw, s); err != nil {
				return Report{}, err
			}
		}
	}
	exp := make([]byte, hBlock)
	failing := 0
	for off := s.Pos; off < size; off += hChunk {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		n := min(hChunk, size-off)
		_, rerr := rw.ReadAt(buf[:n], off)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			// find exactly which blocks can't be read
			all := true
			for i := int64(0); i < n; i += hBlock {
				k := min(hBlock, n-i)
				if _, err := rw.ReadAt(buf[i:i+k], off+i); err != nil && !errors.Is(err, io.EOF) {
					clear(buf[i : i+k])
					s.Bad = addBad(s.Bad, off+i, k)
				} else {
					all = false
					s.checkBlock(buf[i:i+k], off+i, exp)
				}
			}
			if !all {
				failing = 0
			} else if failing++; failing >= hGone {
				if !stillThere() {
					return Report{}, fmt.Errorf("the card stopped answering at %d MB (%w) — run the same command again to continue", off>>20, rerr)
				}
				failing = 0 // present: a dead area, not a pulled card
			}
		} else {
			failing, lastGood = 0, off
			for i := int64(0); i < n; i += hBlock {
				k := min(hBlock, n-i)
				s.checkBlock(buf[i:i+k], off+i, exp)
			}
		}
		progress("Reading it back", off+n)
		if failing == 0 && ((off+n)%healthCheckpoint == 0 || off+n == size) {
			s.Pos = off + n
			if err := save(); err != nil {
				return Report{}, err
			}
		}
	}
	s.Phase = "done"
	save()
	rep := Report{Size: size, Bad: union(s.Bad), Fake: s.Aliased > 0, RealCapacity: size}
	for _, r := range rep.Bad {
		rep.RealCapacity -= r.N
	}
	// a card that silently drops writes past its real size reads back blank there, up to the end
	if k := len(rep.Bad); k > 0 && rep.Bad[k-1].Off+rep.Bad[k-1].N == size && rep.Bad[k-1].N >= hChunk {
		var blank int64
		for _, r := range union(s.Blank) {
			if r.Off >= rep.Bad[k-1].Off {
				blank += r.N
			}
		}
		rep.Fake = rep.Fake || blank*10 >= rep.Bad[k-1].N*9
	}
	return rep, nil
}

// checkBlock compares one block with its pattern; a block holding another offset's pattern
// means writes wrapped around (fake capacity).
func (s *hstate) checkBlock(b []byte, off int64, exp []byte) {
	e := exp[:len(b)]
	fillBlock(e, off, s.Seed)
	if bytes.Equal(b, e) {
		return
	}
	if len(b) >= 16 && binary.LittleEndian.Uint64(b[8:]) == s.Seed {
		if other := int64(binary.LittleEndian.Uint64(b)); other != off && other%hBlock == 0 && other < s.Size {
			fillBlock(e, other, s.Seed)
			if bytes.Equal(b, e) {
				s.Aliased += int64(len(b))
			}
		}
	}
	if uniform(b) {
		s.Blank = addBad(s.Blank, off, int64(len(b)))
	}
	s.Bad = addBad(s.Bad, off, int64(len(b)))
}

func uniform(b []byte) bool {
	for _, c := range b {
		if c != b[0] {
			return false
		}
	}
	return len(b) > 0 && (b[0] == 0 || b[0] == 0xFF)
}

// recheck: before resuming (or after a replug) the card must still hold the pattern written at
// the last checkpoint — another card is never written over or judged by this test (R2).
func recheck(rw RW, s hstate) error {
	at := int64(0)
	if s.Phase == "write" {
		if s.Pos == 0 {
			return nil
		}
		at = s.Pos - hBlock
	}
	if inBad(s.Bad, at) {
		return nil
	}
	b := alignedBuf(hBlock)[:min(hBlock, s.Size-at)] // unbuffered devices need an aligned buffer
	if _, err := rw.ReadAt(b, at); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("checking the card: %w", err)
	}
	// the test's random 64-bit seed in a block header proves it's this test's card (a fake card
	// may hold another offset's block there, which still carries the seed)
	if len(b) < 16 || binary.LittleEndian.Uint64(b[8:]) != s.Seed {
		return ErrDifferentCard
	}
	return nil
}

// alignedBuf returns n bytes starting on a 4096-byte boundary (needed for unbuffered I/O).
func alignedBuf(n int) []byte {
	b := make([]byte, n+4096)
	o := int((4096 - addr(b)%4096) % 4096)
	return b[o : o+n : o+n]
}

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }
