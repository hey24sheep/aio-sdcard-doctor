package salvage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

const (
	KiB = 1 << 10
	MiB = 1 << 20
)

func TestMain(m *testing.M) {
	RetrySleep = 0
	goleak.VerifyTestMain(m)
}

func data(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/4096 + 1)
	}
	return b
}

type rng = faultreader.Range

// run copies src through a fresh Copier and checks the invariants every test shares.
func run(t *testing.T, d []byte, bad []rng, opt faultreader.Options) (Result, []byte, *faultreader.Reader) {
	t.Helper()
	r := faultreader.New(d, bad, opt)
	var out bytes.Buffer
	res, err := NewCopier().Copy(context.Background(), r, int64(len(d)), &out)
	if err != nil {
		t.Fatal(err)
	}
	o := out.Bytes()
	if len(o) != len(d) {
		t.Fatalf("output %d bytes, want %d", len(o), len(d))
	}
	if res.Sum != sha256.Sum256(o) {
		t.Fatal("Sum != sha256(output)")
	}
	for i := range o { // readable bytes preserved, unreadable bytes zero
		if o[i] != d[i] && o[i] != 0 {
			t.Fatalf("byte %d corrupted", i)
		}
	}
	for _, b := range bad {
		for i := b.Off; i < b.Off+b.Len && i < int64(len(o)); i++ {
			if o[i] != 0 {
				t.Fatalf("bad byte %d not zeroed", i)
			}
		}
	}
	var zeroed int64
	for i := range o {
		if o[i] != d[i] {
			zeroed++
		}
	}
	if zeroed > res.Bad {
		t.Fatalf("%d bytes differ but Bad=%d", zeroed, res.Bad)
	}
	return res, o, r
}

func TestNoErrors(t *testing.T) {
	d := data(3*MiB + 17)
	res, o, _ := run(t, d, nil, faultreader.Options{})
	if res.Bad != 0 || !bytes.Equal(o, d) {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestSmallBadSpotMid(t *testing.T) {
	res, _, _ := run(t, data(3*MiB), []rng{{Off: MiB + 40*KiB, Len: 12 * KiB}}, faultreader.Options{})
	if res.Bad != 12*KiB {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestBadFirstBytes(t *testing.T) {
	res, _, _ := run(t, data(2*MiB), []rng{{Off: 0, Len: 10}}, faultreader.Options{})
	if res.Bad != 4096 {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestBadPartialLastBlock(t *testing.T) {
	res, _, _ := run(t, data(4096+1000), []rng{{Off: 4096 + 500, Len: 1}}, faultreader.Options{})
	if res.Bad != 1000 {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestFullyUnreadable20MB(t *testing.T) {
	start := time.Now()
	res, _, r := run(t, data(20*MiB), []rng{{Off: 0, Len: 20 * MiB}}, faultreader.Options{})
	if res.Bad != 20*MiB {
		t.Fatalf("bad=%d", res.Bad)
	}
	if d := time.Since(start); d > 2*time.Second && !raceEnabled {
		t.Fatalf("took %v", d)
	}
	if c := r.Calls(); c >= 400 {
		t.Fatalf("%d read calls", c)
	}
}

func TestDeadZoneThenGood(t *testing.T) {
	res, _, _ := run(t, data(8*MiB), []rng{{Off: 0, Len: 3 * MiB}}, faultreader.Options{})
	if res.Bad < 3*MiB || res.Bad > 3*MiB+64*KiB {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestBadSpanningChunkBoundary(t *testing.T) {
	res, _, _ := run(t, data(3*MiB), []rng{{Off: MiB - 8*KiB, Len: 16 * KiB}}, faultreader.Options{})
	if res.Bad != 16*KiB {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func Test100KBBadMidChunk(t *testing.T) {
	res, _, _ := run(t, data(3*MiB), []rng{{Off: MiB + 300*KiB, Len: 100 * KiB}}, faultreader.Options{})
	if res.Bad < 100*KiB || res.Bad > 164*KiB {
		t.Fatalf("bad=%d", res.Bad)
	}
}

func TestShortRead(t *testing.T) {
	d := data(2*MiB + 100)
	res, o, _ := run(t, d, nil, faultreader.Options{ShortAt: MiB + 50})
	if res.Bad != int64(len(d))-(MiB+50) {
		t.Fatalf("bad=%d", res.Bad)
	}
	if !bytes.Equal(o[:MiB+50], d[:MiB+50]) {
		t.Fatal("readable prefix changed")
	}
}

func TestRetriesCounted(t *testing.T) {
	res, _, _ := run(t, data(MiB), []rng{{Off: 0, Len: 1}}, faultreader.Options{})
	if res.Retried == 0 {
		t.Fatal("Retried = 0 after failed reads")
	}
}

func TestCtxCancelWithinOneBlock(t *testing.T) {
	r := faultreader.New(data(64*MiB), nil, faultreader.Options{Delay: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewCopier().Copy(ctx, r, 64*MiB, &bytes.Buffer{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

type failWriter struct{ after int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("dest broke")
	}
	f.after--
	return len(p), nil
}

func TestDstErrorNoHang(t *testing.T) {
	r := faultreader.New(data(32*MiB), nil, faultreader.Options{})
	start := time.Now()
	_, err := NewCopier().Copy(context.Background(), r, 32*MiB, &failWriter{after: 2})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("dest broke")) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if c := r.Calls(); c > 6 {
		t.Fatalf("kept reading the card after the destination failed: %d calls", c)
	}
}

func TestStalledErrorNotSalvaged(t *testing.T) {
	for _, e := range []error{source.ErrStalled, source.ErrWorkerCrashed, source.ErrTooManyRestarts} {
		r := faultreader.New(data(4*MiB), []rng{{Off: 0, Len: MiB}}, faultreader.Options{Err: e})
		_, err := NewCopier().Copy(context.Background(), r, 4*MiB, &bytes.Buffer{})
		if !errors.Is(err, e) {
			t.Fatalf("err = %v, want %v", err, e)
		}
		if c := r.Calls(); c != 1 {
			t.Fatalf("%v: %d calls, want 1 (no salvage)", e, c)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestCopyAllocsIndependentOfSize(t *testing.T) {
	c := NewCopier()
	small := faultreader.New(data(MiB), nil, faultreader.Options{})
	big := faultreader.New(data(16*MiB), nil, faultreader.Options{})
	a1 := testing.AllocsPerRun(5, func() { c.Copy(context.Background(), small, MiB, discard{}) })
	a2 := testing.AllocsPerRun(5, func() { c.Copy(context.Background(), big, 16*MiB, discard{}) })
	if a2 > a1+20 {
		t.Fatalf("allocs grow with size: %v → %v", a1, a2)
	}
}
