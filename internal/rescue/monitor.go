package rescue

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

const (
	monitorWindow   = 64 << 20 // bytes read per health window
	monitorBadRatio = 0.05
	monitorRun      = 3 // consecutive FAILED/STALLED files
)

// monitor watches the card's health: too many unreadable bytes in the last 64 MiB, or several
// failed files in a row, raise one "card is degrading" question per window.
type monitor struct {
	read, bad int64
	run       int
}

// file records one finished file; it reports whether to warn the user now.
func (m *monitor) file(st manifest.Status, size, bad int64) bool {
	m.read += size
	m.bad += bad
	switch st {
	case manifest.FAILED, manifest.STALLED:
		m.run++
	case manifest.OK, manifest.PARTIAL:
		m.run = 0
	}
	fire := m.run >= monitorRun || (m.read >= monitorWindow/8 && float64(m.bad) > monitorBadRatio*float64(m.read))
	if fire {
		*m = monitor{}
		return true
	}
	if m.read >= monitorWindow {
		m.read, m.bad = 0, 0
	}
	return false
}

// errSkipRest makes the remaining reads of one file fail at once (the user chose
// "skip the bad areas": what was read is kept, the rest is zero-filled).
var errSkipRest = errors.New("skipping the rest of this file's bad areas")

// errStalledRead is a read that stalled inside a file; salvage treats it as unreadable.
var errStalledRead = errors.New("this area of the card stopped responding")

// budgeted counts only the time spent in reads that failed. When a file has spent its budget
// (max(60 s, size ÷ 1 MB/s)) ask() decides: keep trying (reset), skip the rest, or stop (R10).
type budgeted struct {
	r      io.ReaderAt
	now    func() time.Time
	budget time.Duration
	ask    func() string // "continue" | "skip" | "stop"
	stop   func()        // cancels the run (salvage then returns the context error)
	mu     sync.Mutex
	spent  time.Duration
	skip   bool
	asked  int
}

func fileBudget(size int64) time.Duration {
	return max(60*time.Second, time.Duration(size/1_000_000)*time.Second)
}

func (b *budgeted) ReadAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	skip := b.skip
	b.mu.Unlock()
	if skip {
		return 0, errSkipRest
	}
	start := b.now()
	n, err := b.r.ReadAt(p, off)
	if err == nil || err == io.EOF {
		return n, err
	}
	if errors.Is(err, source.ErrStalled) {
		// The reader was restarted after a read that never answered: treat that area like a bad
		// sector (zero-filled, file saved as PARTIAL) instead of dropping the whole file (I8).
		err = errStalledRead
	}
	b.mu.Lock()
	b.spent += b.now().Sub(start)
	over := b.spent > b.budget
	b.mu.Unlock()
	if over {
		b.asked++
		switch b.ask() {
		case "skip":
			b.mu.Lock()
			b.skip = true
			b.mu.Unlock()
		case "stop":
			b.stop()
		default:
			b.mu.Lock()
			b.spent = 0
			b.mu.Unlock()
		}
	}
	return n, err
}
