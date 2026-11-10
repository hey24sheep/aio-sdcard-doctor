package imaging

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
)

// swapDev becomes a different card (same size) when it is reopened.
type swapDev struct {
	fakeDev
	other []byte
}

func (d *swapDev) Reopen(p string) error {
	d.fakeDev.Reopen(p)
	d.r = faultreader.New(d.other, nil, faultreader.Options{})
	return nil
}

// C1: after the card comes back, its content must be the same card's before imaging continues.
func TestImageReopenChecksFingerprint(t *testing.T) {
	data := devData(8 * MiB)
	other := devData(8 * MiB)
	other[10] ^= 0xFF
	var mu sync.Mutex
	gone, calls := false, 0
	d := &swapDev{fakeDev: fakeDev{size: int64(len(data))}, other: other}
	d.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls == 3 {
			gone = true
		}
		if gone {
			return errors.New("device not ready")
		}
		return nil
	}})
	im := newImage(t, &d.fakeDev)
	im.OpenDevice = func() (Device, error) { return d, nil }
	im.Present = func(drives.Identity, string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if gone {
			gone = false
			return "another-device", true
		}
		return "fake", true
	}
	_, err := run(t, im, context.Background())
	if err == nil || !strings.Contains(err.Error(), "different card") {
		t.Fatalf("err = %v", err)
	}
}

// presence checks are throttled: bad sectors on a present card don't re-list every disk (I12).
func TestImagePresenceChecksThrottled(t *testing.T) {
	data := devData(8 * MiB)
	var bad []faultreader.Range
	for off := int64(MiB); off < 7*MiB; off += 64 << 10 {
		bad = append(bad, faultreader.Range{Off: off, Len: 4096})
	}
	dev := &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	n := 0
	im.Present = func(drives.Identity, string) (string, bool) { n++; return "fake", true }
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	if n > 3 {
		t.Fatalf("%d presence checks for one run", n)
	}
}

// orderFS records the order of image writes, image fsyncs and map saves.
type orderFS struct {
	destfs.FS
	mu  sync.Mutex
	log []string
}

func (o *orderFS) add(s string) { o.mu.Lock(); o.log = append(o.log, s); o.mu.Unlock() }

func (o *orderFS) Create(p string) (destfs.File, error) {
	if strings.Contains(p, ".map") {
		o.add("map")
	}
	return o.FS.Create(p)
}

func (o *orderFS) OpenImage(p string, size int64, resume bool) (destfs.ImageFile, error) {
	f, err := o.FS.OpenImage(p, size, resume)
	if err != nil {
		return nil, err
	}
	return &orderImg{ImageFile: f, o: o}, nil
}

type orderImg struct {
	destfs.ImageFile
	o *orderFS
}

func (f *orderImg) WriteAt(b []byte, off int64) (int, error) {
	f.o.add("write")
	return f.ImageFile.WriteAt(b, off)
}
func (f *orderImg) Sync() error { f.o.add("sync"); return f.ImageFile.Sync() }

// I10: the map never claims data that has not been flushed to the image file.
func TestImageMapSavedOnlyAfterSync(t *testing.T) {
	data := devData(4 * MiB)
	dev := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	im.SaveEvery = time.Nanosecond
	of := &orderFS{FS: destfs.New(dirOf(im.Out))}
	env := job.Env{Prompt: job.AutoPrompter{Yes: true}, Out: of}
	p, err := im.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	if _, err := im.Run(context.Background(), env, p); err != nil {
		t.Fatal(err)
	}
	dirty := false
	for _, e := range of.log {
		switch e {
		case "write":
			dirty = true
		case "sync":
			dirty = false
		case "map":
			if dirty {
				t.Fatalf("map saved over unflushed image data: %v", of.log)
			}
		}
	}
}

func dirOf(p string) string { return p[:strings.LastIndexAny(p, `/\`)] }
