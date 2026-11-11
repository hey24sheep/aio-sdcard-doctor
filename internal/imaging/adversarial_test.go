package imaging

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

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
