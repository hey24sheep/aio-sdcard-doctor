package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
)

// flakyFS fails one write to a ZIP part (the disk was full for a moment), then works again.
type flakyFS struct {
	destfs.FS
	mu    sync.Mutex
	fails int
}

type flakyFile struct {
	destfs.File
	fs *flakyFS
}

func (f *flakyFS) Create(p string) (destfs.File, error) {
	w, err := f.FS.Create(p)
	if err != nil || !strings.HasSuffix(p, ".zip.tmp") {
		return w, err
	}
	return &flakyFile{File: w, fs: f}, nil
}

func (f *flakyFile) Write(b []byte) (int, error) {
	f.fs.mu.Lock()
	fail := f.fs.fails > 0
	if fail {
		f.fs.fails--
	}
	f.fs.mu.Unlock()
	if fail {
		return 0, errors.New("There is not enough space on the disk.")
	}
	return f.File.Write(b)
}

// Final review I1: after one failed write, later files still go into a (new) part; the files of
// the broken part are reported failed (so the next run copies them again), never kept as stored.
func TestZipPartRecoversAfterWriteError(t *testing.T) {
	dir := t.TempDir()
	fs := &flakyFS{FS: destfs.New(dir)}
	s := ZipSink(fs, dir, store(t, dir))
	big := make([]byte, 300<<10) // incompressible and larger than the zip writer's buffer: reaches the file
	rand.New(rand.NewSource(1)).Read(big)
	put := func(i int) error {
		_, _, err := s.Put(context.Background(), fmt.Sprintf("Photos/f%d.bin", i), time.Time{}, writer(append([]byte{byte(i)}, big...), 0), always)
		return err
	}
	if err := put(1); err != nil {
		t.Fatal(err)
	}
	fs.fails = 1
	if err := put(2); err == nil {
		t.Fatal("the failed write wasn't reported")
	}
	for i := 3; i <= 5; i++ { // the disk has room again
		if err := put(i); err != nil {
			t.Fatalf("file %d after the error: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	failed := strings.Join(s.(interface{ Failed() []string }).Failed(), " ")
	if !strings.Contains(failed, "f1.bin") {
		t.Fatalf("the broken part's earlier file isn't reported failed: %q", failed)
	}
	got := map[string][]byte{}
	parts, _ := filepath.Glob(filepath.Join(dir, "Photos-*.zip"))
	for _, p := range parts {
		for k, v := range readZip(t, p) {
			got[k] = v
		}
	}
	for i := 3; i <= 5; i++ {
		if _, ok := got[fmt.Sprintf("f%d.bin", i)]; !ok {
			t.Fatalf("f%d.bin missing from the new part: %v", i, len(got))
		}
	}
}

// Final review I1 (FAT32): a file that would push the open part past the part size starts a new
// part, so a part is never "1 GiB + one big file" (over 4 GiB on FAT32).
func TestBigFileStartsNewPart(t *testing.T) {
	old := MaxPartBytes
	MaxPartBytes = 64 << 10
	defer func() { MaxPartBytes = old }()
	dir := t.TempDir()
	s := ZipSink(destfs.New(dir), dir, store(t, dir))
	small := bytes.Repeat([]byte{1}, 10<<10)
	large := make([]byte, 100<<10)
	for i := range large {
		large[i] = byte(i * 7919 >> 3) // poorly compressible
	}
	s.Put(context.Background(), "Videos/a.bin", time.Time{}, writer(small, 0), always)
	s.Put(context.Background(), "Videos/big.bin", time.Time{}, writer(large, 0), always)
	s.Close()
	if p1 := readZip(t, filepath.Join(dir, "Videos-0001.zip")); len(p1) != 1 {
		t.Fatalf("part 1 holds %d files: the big file was added to it", len(p1))
	}
	if p2 := readZip(t, filepath.Join(dir, "Videos-0002.zip")); len(p2) != 1 {
		t.Fatal("the big file isn't in its own part")
	}
}

// Final review I4: compressing a big image of photos (incompressible) must not hold whole
// segments in memory several times over; peak heap stays bounded whatever the image size.
func TestCompressImageMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("compresses 128 MiB")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	if err := os.WriteFile(img, randomBytes(128<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // heap ≈ what is live, not GC timing
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapInuse, ms.HeapInuse
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapInuse)
		}
	}()
	start := time.Now()
	_, err := CompressImage(context.Background(), destfs.New(dir), img, img+".xz", true, nil)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	used := int64(peak-base) >> 20
	t.Logf("peak heap +%d MiB, %v (%.0f MB/s)", used, time.Since(start), 128/time.Since(start).Seconds())
	if used > 150 {
		t.Fatalf("peak heap +%d MiB compressing 128 MiB", used)
	}
}

// Final review I4: photos don't compress, and LZMA crawls on them (~0.4 MB/s); such data is
// stored in the .xz instead, at copy speed. Mixed content still round-trips exactly, and what
// does compress still shrinks.
func TestCompressImageStoresIncompressibleFast(t *testing.T) {
	text := bytes.Repeat([]byte("DCIM/100MEDIA/IMG_0001.JPG 2026-11-10 4.1 MB\n"), (6<<20)/45)
	data := append(append(randomBytes(20<<20), make([]byte, 6<<20)...), text...)
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	os.WriteFile(img, data, 0o644)
	start := time.Now()
	if _, err := CompressImage(context.Background(), destfs.New(dir), img, img+".xz", true, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("took %v for 32 MiB", d)
	}
	f, _ := os.Open(img + ".xz")
	defer f.Close()
	st, _ := f.Stat()
	if st.Size() > 21<<20 {
		t.Fatalf(".xz is %d bytes: the zeros and text weren't compressed", st.Size())
	}
	r, err := xz.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: err %v, equal %v", err, bytes.Equal(got, data))
	}
}

// slowWriter holds its first write until the run is cancelled, like a slow USB disk.
type slowWriter struct {
	ctx   context.Context
	first bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.first {
		w.first = true
		<-w.ctx.Done()
		time.Sleep(200 * time.Millisecond) // the scheduler has filled the rest by now
	}
	return len(p), nil
}

// Re-review #1: when the workers finish ahead of a slow destination, Stop (cancel) must end
// the compression, not hang it.
func TestCompressCancelWithSlowWriterReturns(t *testing.T) {
	defer func(s int64) { segment = s }(segment)
	segment = 1 << 20
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2)) // one worker: deterministic order
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	if err := os.WriteFile(img, randomBytes(6<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- compressParallel(ctx, img, 6<<20, &slowWriter{ctx: ctx}, nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("compression did not return after cancel")
	}
}
