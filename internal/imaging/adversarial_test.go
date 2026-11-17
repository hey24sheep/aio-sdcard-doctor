package imaging

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
)

// staleDev is a card whose open handle went dead (a USB glitch, a quick reseat at the same
// drive number): every read fails until it is reopened.
type staleDev struct {
	*fakeDev
	reads, staleAfter int
	reopened          bool
}

func (d *staleDev) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	d.reads++
	dead := d.reads > d.staleAfter && !d.reopened
	d.mu.Unlock()
	if dead {
		return 0, errors.New("the device is not ready")
	}
	return d.fakeDev.ReadAt(p, off)
}

func (d *staleDev) Reopen(p string) error {
	d.mu.Lock()
	d.reopened = true
	d.mu.Unlock()
	return d.fakeDev.Reopen(p)
}

// Adversarial pass C3: a card back at the same device path was never reopened, so a dead handle
// turned the rest of the card into "unreadable" zeros the card could have supplied.
func TestStaleHandleAtSamePathIsReopened(t *testing.T) {
	data := devData(16 * MiB)
	fd := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	dev := &staleDev{fakeDev: fd, staleAfter: 3}
	im := newImage(t, fd)
	im.OpenDevice = func() (Device, error) { return dev, nil }
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	m := readMap(t, im.Out+".map")
	if bad := m.Bytes(BadSector) + m.Bytes(NonTried) + m.Bytes(NonTrimmed) + m.Bytes(NonScraped); bad != 0 || !dev.reopened {
		t.Fatalf("%d bytes left unread, reopened %v", bad, dev.reopened)
	}
	if got, _ := os.ReadFile(im.Out); !bytes.Equal(got, data) {
		t.Fatal("image differs from the card")
	}
}

// Adversarial pass C4: a map whose pass number is ahead of areas still untried (an interrupted
// GNU ddrescue run writes its own pass numbers) skipped copying and was called complete.
func TestMapPassAheadOfUntriedAreasIsFinished(t *testing.T) {
	data := devData(8 * MiB)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	dev := &fakeDev{size: int64(len(data))}
	dev.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		if calls++; calls == 3 {
			cancel()
		}
		return nil
	}})
	im := newImage(t, dev)
	if _, err := run(t, im, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run: %v", err)
	}
	b, _ := os.ReadFile(im.Out + ".map")
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "" {
			f := strings.Fields(l)
			lines[i] = f[0] + "     " + f[1] + "               3" // ddrescue was in its trimming pass
			break
		}
	}
	os.WriteFile(im.Out+".map", []byte(strings.Join(lines, "\n")), 0o644)
	if readMap(t, im.Out+".map").Bytes(NonTried) == 0 {
		t.Fatal("setup: nothing left untried")
	}
	im2 := newImage(t, &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))})
	im2.Out = im.Out
	if _, err := run(t, im2, context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := readMap(t, im.Out+".map"); m.Bytes(NonTried) != 0 {
		t.Fatalf("%d bytes still untried after a 'complete' run", m.Bytes(NonTried))
	}
	if got, _ := os.ReadFile(im.Out); !bytes.Equal(got, data) {
		t.Fatal("image differs from the card")
	}
}

// Adversarial pass C9: a power cut between "map → map.bak" and "new map in place" left only the
// .bak, and the image could not be resumed ("file already exists").
func TestResumeFromBakAfterPowerCut(t *testing.T) {
	data := devData(8 * MiB)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	dev := &fakeDev{size: int64(len(data))}
	dev.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		if calls++; calls == 3 {
			cancel()
		}
		return nil
	}})
	im := newImage(t, dev)
	run(t, im, ctx)
	for _, p := range []string{im.Out + ".map", im.Out + ".json"} {
		os.Rename(p, p+".bak") // the moment between the two renames
	}
	im2 := newImage(t, &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))})
	im2.Out = im.Out
	if _, err := run(t, im2, context.Background()); err != nil {
		t.Fatalf("couldn't resume from the .bak files: %v", err)
	}
	if got, _ := os.ReadFile(im.Out); !bytes.Equal(got, data) {
		t.Fatal("image differs from the card")
	}
}

// Adversarial pass 2 SR3: imaging read 4 KiB at the finest, so one bad 512-byte sector
// zero-filled the seven readable sectors next to it; bad areas cost only their own sectors now.
func TestOneBadSectorCostsOnlyItself(t *testing.T) {
	data := devData(4 * MiB)
	bad := []faultreader.Range{{Off: MiB + 4096 + 1024, Len: 512}, {Off: 3*MiB + 1536, Len: 8192 + 1024}}
	dev := &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	im.RetryPasses = 1
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	m := readMap(t, im.Out+".map")
	if got := m.Bytes(BadSector); got != 512+8192+1024 {
		t.Fatalf("bad bytes %d, map %+v", got, m.Blocks)
	}
	got, _ := os.ReadFile(im.Out)
	for i := range got {
		inBad := false
		for _, r := range bad {
			inBad = inBad || int64(i) >= r.Off && int64(i) < r.Off+r.Len
		}
		if inBad && got[i] != 0 || !inBad && got[i] != data[i] {
			t.Fatalf("byte %d wrong", i)
		}
	}
}

func blockedBy(p job.Plan, kind string) *job.Issue {
	for i, is := range p.Issues {
		if is.Kind == kind && is.Severity == job.Block {
			return &p.Issues[i]
		}
	}
	return nil
}

// Adversarial pass 2 M1: card.img.map left without card.img (deleted, or a finished compressed
// image) passed Plan, and Run then failed with a bare OS error.
func TestImageLeftoverMapIsExplained(t *testing.T) {
	data := devData(MiB)
	im := newImage(t, &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))})
	os.WriteFile(im.Out+".map", []byte("0x0 + 1\n0x0 0x100000 +\n"), 0o644)
	os.WriteFile(im.Out+".xz", []byte("finished"), 0o644)
	defer im.Close()
	p, err := im.Plan(context.Background(), job.Env{})
	if err != nil {
		t.Fatal(err)
	}
	is := blockedBy(p, "image-map-left")
	if is == nil || !strings.Contains(is.Detail, ".map") || !strings.Contains(is.Detail, "can't be resumed") {
		t.Fatalf("issues %+v", p.Issues)
	}
}

// M2: a map that can't be read (too large, damaged) was ignored by Plan; Run then started over
// and failed on the existing image file.
func TestImageUnreadableMapIsExplained(t *testing.T) {
	data := devData(MiB)
	im := newImage(t, &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))})
	os.WriteFile(im.Out, make([]byte, len(data)), 0o644)
	os.WriteFile(im.Out+".map", []byte("not a map\n"), 0o644)
	defer im.Close()
	p, err := im.Plan(context.Background(), job.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if is := blockedBy(p, "image-map-unreadable"); is == nil {
		t.Fatalf("issues %+v", p.Issues)
	}
}
