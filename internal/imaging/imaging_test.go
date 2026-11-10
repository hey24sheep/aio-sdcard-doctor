package imaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

const envWorker = "AIO_IMAGING_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(envWorker) == "1" {
		os.Exit(worker.Main())
	}
	worker.Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1")
		return c
	}
	salvage.RetrySleep = 0
	goleak.VerifyTestMain(m)
}

const MiB = 1 << 20

type fakeDev struct {
	mu      sync.Mutex
	r       *faultreader.Reader
	size    int64
	id      drives.Identity
	reopens int
}

func (d *fakeDev) ReadAt(p []byte, off int64) (int, error) { return d.r.ReadAt(p, off) }
func (d *fakeDev) Size() int64                             { return d.size }
func (d *fakeDev) Identity() drives.Identity               { return d.id }
func (d *fakeDev) Reopen(string) error                     { d.mu.Lock(); d.reopens++; d.mu.Unlock(); return nil }
func (d *fakeDev) Close() error                            { return nil }

func devData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/4096 + 1)
	}
	return b
}

func newImage(t *testing.T, dev *fakeDev) *Image {
	if dev.id.PhysicalID == "" {
		// the per-card lock is machine-wide: a key shared with another test package's fake card
		// makes them block each other. PhysicalID changes only the lock key (Matches ignores it).
		dev.id.PhysicalID = "fake-" + t.Name()
	}
	out := filepath.Join(t.TempDir(), "card.img")
	return &Image{Device: "fake", Out: out,
		OpenDevice: func() (Device, error) { return dev, nil },
		Present:    func(drives.Identity, string) (string, bool) { return "fake", true },
		LookupDrive: func(string) (drives.Drive, error) {
			return drives.Drive{FS: "NTFS", Free: 1 << 40, PhysicalID: "disk"}, nil
		},
		PollEvery: time.Millisecond, WaitTimeout: time.Second,
	}
}

func run(t *testing.T, im *Image, ctx context.Context) (job.Result, error) {
	t.Helper()
	env := job.Env{Prompt: job.AutoPrompter{Yes: true}}
	p, err := im.Plan(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	return im.Run(ctx, env, p)
}

func readMap(t *testing.T, p string) *Mapfile {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseMapfile(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestImageCleanDevice(t *testing.T) {
	data := devData(32 * MiB)
	dev := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(im.Out)
	if !bytes.Equal(got, data) {
		t.Fatal("image differs from device")
	}
	m := readMap(t, im.Out+".map")
	if len(m.Blocks) != 1 || m.Blocks[0].Status != Finished || m.CurStatus != Finished {
		t.Fatalf("%+v", m)
	}
	var meta Meta
	b, _ := os.ReadFile(im.Out + ".json")
	json.Unmarshal(b, &meta)
	if !meta.Complete || meta.SHA256 != fmt.Sprintf("%X", sha256.Sum256(data)) || meta.BadBytes != 0 {
		t.Fatalf("%+v", meta)
	}
}

func TestImageBadAreas(t *testing.T) {
	data := devData(16 * MiB)
	bad := []faultreader.Range{{Off: 3*MiB + 8192, Len: 4096}, {Off: 9 * MiB, Len: 3 * 4096}}
	dev := &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	res, err := run(t, im, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := readMap(t, im.Out+".map")
	if got := m.Bytes(BadSector); got != 4*4096 {
		t.Fatalf("bad bytes %d, map %+v", got, m.Blocks)
	}
	for _, b := range m.Blocks {
		if b.Status != Finished && b.Status != BadSector {
			t.Fatalf("unfinished block %+v", b)
		}
	}
	got, _ := os.ReadFile(im.Out)
	for i := range got {
		inBad := false
		for _, r := range bad {
			inBad = inBad || (int64(i) >= r.Off && int64(i) < r.Off+r.Len)
		}
		if (inBad && got[i] != 0) || (!inBad && got[i] != data[i]) {
			t.Fatalf("byte %d wrong", i)
		}
	}
	if res.Clean {
		t.Fatal("image with bad sectors reported clean")
	}
}

func TestImageDeadDeviceBounded(t *testing.T) {
	data := devData(64 * MiB)
	dev := &fakeDev{r: faultreader.New(data, []faultreader.Range{{Off: 0, Len: 64 * MiB}}, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := dev.r.Calls(); c > 64*MiB/4096+2000 {
		t.Fatalf("%d reads", c)
	}
	if m := readMap(t, im.Out+".map"); m.Bytes(BadSector) != 64*MiB {
		t.Fatalf("%+v", m.Blocks)
	}
}

// TestImageResumeEachPass interrupts the run at different points; resuming must end with the
// same map and image as an uninterrupted run.
func TestImageResumeEachPass(t *testing.T) {
	data := devData(8 * MiB)
	bad := []faultreader.Range{{Off: 2 * MiB, Len: 300_000}, {Off: 6 * MiB, Len: 4096}}
	ref := &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))}
	refIm := newImage(t, ref)
	run(t, refIm, context.Background())
	want := readMap(t, refIm.Out+".map")
	for _, stopAt := range []int64{3, 40, 120, 300} {
		t.Run(fmt.Sprint(stopAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var calls int64
			dev := &fakeDev{size: int64(len(data))}
			dev.r = faultreader.New(data, bad, faultreader.Options{OnRead: func(int64, int) error {
				if calls++; calls == stopAt {
					cancel()
				}
				return nil
			}})
			im := newImage(t, dev)
			if _, err := run(t, im, ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run: %v", err)
			}
			im2 := newImage(t, &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))})
			im2.Out = im.Out
			if _, err := run(t, im2, context.Background()); err != nil {
				t.Fatal(err)
			}
			got := readMap(t, im.Out+".map")
			if fmt.Sprint(got.Blocks) != fmt.Sprint(want.Blocks) {
				t.Fatalf("resumed map %+v\nwant %+v", got.Blocks, want.Blocks)
			}
		})
	}
}

func TestImageResumeFingerprintMismatch(t *testing.T) {
	data := devData(8 * MiB)
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	dev := &fakeDev{size: int64(len(data))}
	dev.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		if calls++; calls == 4 {
			cancel()
		}
		return nil
	}})
	im := newImage(t, dev)
	run(t, im, ctx)
	other := devData(8 * MiB)
	other[100] ^= 0xFF // same size and model, different content: another card
	im2 := newImage(t, &fakeDev{r: faultreader.New(other, nil, faultreader.Options{}), size: int64(len(other))})
	im2.Out = im.Out
	_, err := run(t, im2, context.Background())
	if err == nil || !strings.Contains(err.Error(), "different card") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageUnplugResume(t *testing.T) {
	data := devData(8 * MiB)
	var mu sync.Mutex
	gone, polls := false, 0
	var calls int
	dev := &fakeDev{size: int64(len(data))}
	dev.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls == 3 && dev.reopens == 0 {
			gone = true
		}
		if gone {
			return errors.New("device not ready")
		}
		return nil
	}})
	im := newImage(t, dev)
	im.Present = func(drives.Identity, string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !gone {
			return "fake", true
		}
		if polls++; polls < 3 {
			return "", false
		}
		gone = false
		return "fake2", true
	}
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(im.Out)
	if !bytes.Equal(got, data) || dev.reopens != 1 {
		t.Fatalf("image differs or reopens=%d", dev.reopens)
	}
}

func TestImageCompressed(t *testing.T) {
	data := devData(4 * MiB)
	dev := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	im.Compress = true
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(im.Out); !os.IsNotExist(err) {
		t.Fatal("raw image kept")
	}
	f, err := os.Open(im.Out + ".xz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, _ := xz.NewReader(f)
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, data) {
		t.Fatal(".img.xz doesn't decompress to the device")
	}
}

// TestImageThenRescue images a FAT32 card, then rescues from the image file (no admin, no mount).
func TestImageThenRescue(t *testing.T) {
	card := testutil.AndroidCard(true)
	img := testutil.FAT32Image(t, card, 64<<20)
	data, _ := os.ReadFile(img)
	dev := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	r := &rescue.Rescue{From: im.Out, Raw: true, Dest: dst, StallTimeout: 10 * time.Second,
		LookupDrive: func(p string) (drives.Drive, error) {
			return drives.Drive{FS: "NTFS", Free: 1 << 40, PhysicalID: p, Removable: true}, nil
		}}
	env := job.Env{Prompt: job.AutoPrompter{Yes: true}}
	p, err := r.Plan(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Run(context.Background(), env, p); err != nil {
		t.Fatal(err)
	}
	s, _ := job.OpenStore(filepath.Join(dst, "Reports", "state.db"))
	defer s.Close()
	m, _ := manifest.Open(s)
	for rel, f := range card {
		e, ok, _ := m.Get(rel)
		if !ok || e.Status != manifest.OK || e.SHA256 != fmt.Sprintf("%X", sha256.Sum256(f.Data)) {
			t.Errorf("%s: %+v", rel, e)
		}
	}
}
