package fix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
)

// memCard is a card in memory. real < size makes it a fake-capacity card: writes past real
// wrap around onto the start, like the cheap cards that lie about their size.
type memCard struct {
	mu         sync.Mutex
	b          []byte
	size, real int64
	badFrom    int64 // reads in [badFrom, badTo) fail
	badTo      int64
	writes     int
	failAfter  int   // writes after this many fail with errUnplugged (0 = never)
	wbadFrom   int64 // writes in [wbadFrom, wbadTo) fail
	wbadTo     int64
	pulledFrom int64 // > 0: every read at or past this offset fails (card pulled while reading)
	dropFrom   int64 // > 0: writes past this are silently dropped and read back as zeros (a fake)
	gone       bool  // pulled: reads fail everywhere
}

var errUnplugged = errors.New("device gone")

func newMem(size, real int64) *memCard {
	if real == 0 {
		real = size
	}
	return &memCard{b: make([]byte, real), size: size, real: real}
}

func (m *memCard) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.failAfter > 0 && m.writes > m.failAfter {
		return 0, errUnplugged
	}
	if off < m.wbadTo && off+int64(len(p)) > m.wbadFrom {
		return 0, errors.New("write error")
	}
	for i := range p {
		if m.dropFrom > 0 && off+int64(i) >= m.dropFrom {
			continue
		}
		m.b[(off+int64(i))%m.real] = p[i]
	}
	return len(p), nil
}

func (m *memCard) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// a pulled card answers nothing: once unplugged (writes past failAfter, or a read reaching
	// pulledFrom), every read fails until the test puts it back
	unplugged := m.failAfter > 0 && m.writes > m.failAfter
	if m.pulledFrom > 0 && (m.gone || off+int64(len(p)) > m.pulledFrom) {
		m.gone, unplugged = true, true
	}
	if m.pulledFrom == 0 {
		m.gone = false
	}
	if unplugged {
		return 0, errUnplugged
	}
	if off < m.badTo && off+int64(len(p)) > m.badFrom {
		return 0, errors.New("I/O error")
	}
	for i := range p {
		if m.dropFrom > 0 && off+int64(i) >= m.dropFrom {
			p[i] = 0
			continue
		}
		p[i] = m.b[(off+int64(i))%m.real]
	}
	return len(p), nil
}

func henv() job.Env {
	return job.Env{Prompt: job.AutoPrompter{Yes: true}, Progress: func(job.Progress) {}, Now: time.Now, Log: func(string, string) {}}
}

func hopts(t *testing.T) HealthOptions {
	return HealthOptions{StorePath: filepath.Join(t.TempDir(), "health.db")}
}

func TestHealthTestGoodImage(t *testing.T) {
	// a real image file, as the test runs on one card image
	p := filepath.Join(t.TempDir(), "card.img")
	f, _ := os.Create(p)
	f.Truncate(24 << 20)
	defer f.Close()
	rep, err := HealthTest(context.Background(), f, 24<<20, henv(), hopts(t))
	if err != nil || len(rep.Bad) != 0 || rep.Fake || rep.RealCapacity != 24<<20 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestHealthTestFakeCapacity(t *testing.T) {
	c := newMem(32<<20, 8<<20) // says 32 MB, holds 8 MB
	rep, err := HealthTest(context.Background(), c, c.size, henv(), hopts(t))
	if err != nil || !rep.Fake || rep.RealCapacity != 8<<20 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestHealthTestBadRegion(t *testing.T) {
	c := newMem(16<<20, 0)
	c.badFrom, c.badTo = 5<<20, 5<<20+4096
	rep, err := HealthTest(context.Background(), c, c.size, henv(), hopts(t))
	if err != nil || rep.Fake || len(rep.Bad) != 1 || rep.Bad[0].Off > 5<<20 || rep.Bad[0].Off+rep.Bad[0].N < 5<<20+4096 {
		t.Fatalf("%+v %v", rep, err)
	}
	if rep.RealCapacity >= 16<<20 {
		t.Fatalf("bad area not taken off the capacity: %d", rep.RealCapacity)
	}
}

func TestHealthTestResume(t *testing.T) {
	old := healthCheckpoint
	healthCheckpoint = 2 << 20
	defer func() { healthCheckpoint = old }()
	c := newMem(16<<20, 0)
	c.failAfter = 9 // unplugged during the write phase
	o := hopts(t)
	if _, err := HealthTest(context.Background(), c, c.size, henv(), o); err == nil {
		t.Fatal("no error when the card went away")
	}
	c.failAfter = 0
	writesBefore := c.writes
	rep, err := HealthTest(context.Background(), c, c.size, henv(), o)
	if err != nil || len(rep.Bad) != 0 || rep.Fake {
		t.Fatalf("%+v %v", rep, err)
	}
	if done := c.writes - writesBefore; done >= 16 {
		t.Fatalf("resume rewrote everything (%d chunk writes)", done)
	}
}

func TestHealthResumeRegatesAndRechecks(t *testing.T) {
	old := healthCheckpoint
	healthCheckpoint = 2 << 20
	defer func() { healthCheckpoint = old }()
	a := newMem(16<<20, 0)
	a.failAfter = 9
	o := hopts(t)
	HealthTest(context.Background(), a, a.size, henv(), o)
	b := newMem(16<<20, 0) // another card of the same size put in
	if _, err := HealthTest(context.Background(), b, b.size, henv(), o); !errors.Is(err, ErrDifferentCard) {
		t.Fatalf("err %v", err)
	}
}

func TestHealthCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newMem(8<<20, 0)
	if _, err := HealthTest(ctx, c, c.size, henv(), hopts(t)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHealthReplugAsked(t *testing.T) {
	c := newMem(4<<20, 0)
	p := &askLog{}
	e := henv()
	e.Prompt = p
	o := hopts(t)
	o.Replug = true
	reopened := false
	o.Reopen = func() (RW, error) { reopened = true; return c, nil }
	if _, err := HealthTest(context.Background(), c, c.size, e, o); err != nil {
		t.Fatal(err)
	}
	if p.kinds["replug"] != 1 || !reopened || !p.saidDoNotFormat {
		t.Fatalf("asked %v reopened %v warned %v", p.kinds, reopened, p.saidDoNotFormat)
	}
}

type askLog struct {
	kinds           map[string]int
	saidDoNotFormat bool
}

func (a *askLog) Ask(_ context.Context, is job.Issue) job.Choice {
	if a.kinds == nil {
		a.kinds = map[string]int{}
	}
	a.kinds[is.Kind]++
	a.saidDoNotFormat = a.saidDoNotFormat || containsAll(is.Detail, "do not format")
	return job.Continue
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !contains(s, x) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestHealthUsesUnbufferedFlags: the raw device is opened past the OS cache (R7), per OS.
func TestHealthUsesUnbufferedFlags(t *testing.T) {
	if err := unbufferedOK(); err != nil {
		t.Fatal(err)
	}
}

func TestAlignedBuffer(t *testing.T) {
	for i := 0; i < 20; i++ {
		b := alignedBuf(1 << 20)
		if len(b) != 1<<20 || addr(b)%4096 != 0 {
			t.Fatalf("len %d addr %% 4096 = %d", len(b), addr(b)%4096)
		}
	}
}

func TestUnionCountsOnce(t *testing.T) {
	got := union([]Range{{8192, 4096}, {0, 4096}, {8192, 8192}, {4096, 4096}})
	if len(got) != 1 || got[0] != (Range{0, 16384}) {
		t.Fatalf("%v", got)
	}
}
