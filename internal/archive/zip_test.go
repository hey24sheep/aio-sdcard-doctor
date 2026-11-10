package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
)

func store(t *testing.T, dir string) *job.Store {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "Reports"), 0o755)
	s, err := job.OpenStore(filepath.Join(dir, "Reports", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func readZip(t *testing.T, p string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = b
	}
	return out
}

func TestZipSinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := ZipSink(destfs.New(dir), dir, store(t, dir))
	mt := time.Date(2024, 3, 4, 5, 6, 8, 0, time.Local)
	text := bytes.Repeat([]byte("compressible text "), 5000)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF}, randomBytes(100_000)...)
	for dest, data := range map[string][]byte{"Documents/Docs/a.txt": text, "Photos/DCIM/b.jpg": jpeg} {
		stored, _, err := s.Put(context.Background(), dest, mt, writer(data, 0), always)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{"Documents/Docs/a.txt": "Documents-0001.zip/Docs/a.txt", "Photos/DCIM/b.jpg": "Photos-0001.zip/DCIM/b.jpg"}[dest]; stored != want {
			t.Fatalf("stored %q want %q", stored, want)
		}
	}
	if s.Has("Photos-0001.zip/DCIM/b.jpg", int64(len(jpeg))) {
		t.Fatal("entry reported stored before its part was closed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.Has("Photos-0001.zip/DCIM/b.jpg", int64(len(jpeg))) {
		t.Fatal("closed part not indexed")
	}
	docs := readZip(t, filepath.Join(dir, "Documents-0001.zip"))
	if !bytes.Equal(docs["Docs/a.txt"], text) {
		t.Fatal("text entry differs")
	}
	zr, _ := zip.OpenReader(filepath.Join(dir, "Photos-0001.zip"))
	defer zr.Close()
	f := zr.File[0]
	if f.Method != zip.Store || !f.Modified.Equal(mt) {
		t.Fatalf("jpeg entry method %d modified %v", f.Method, f.Modified)
	}
	if st, _ := os.Stat(filepath.Join(dir, "Documents-0001.zip")); st.Size() > int64(len(text))/10 {
		t.Fatalf("text not compressed: %d bytes", st.Size())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("tmp left: %v", m)
	}
}

func TestZipSinkPartRollover(t *testing.T) {
	dir := t.TempDir()
	s := ZipSink(destfs.New(dir), dir, store(t, dir))
	for i := 0; i < MaxEntries+1; i++ {
		if _, _, err := s.Put(context.Background(), fmt.Sprintf("Other/f%04d.bin", i), time.Time{}, writer([]byte{byte(i)}, 0), always); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if len(readZip(t, filepath.Join(dir, "Other-0001.zip"))) != MaxEntries || len(readZip(t, filepath.Join(dir, "Other-0002.zip"))) != 1 {
		t.Fatal("rollover wrong")
	}
}

func TestZipSinkCrashMidPart(t *testing.T) {
	dir := t.TempDir()
	st := store(t, dir)
	s := ZipSink(destfs.New(dir), dir, st)
	s.Put(context.Background(), "Other/a.bin", time.Time{}, writer([]byte("a"), 0), always)
	s.(*zipSink).abandon() // the process "crashed": the open part is left as .zip.tmp, unfinished
	s2 := ZipSink(destfs.New(dir), dir, st)
	if m, _ := filepath.Glob(filepath.Join(dir, "*.zip.tmp")); len(m) != 0 {
		t.Fatalf("leftover part not removed on resume: %v", m)
	}
	if s2.Has("Other-0001.zip/a.bin", 1) {
		t.Fatal("entry of a lost part reported stored")
	}
	stored, _, err := s2.Put(context.Background(), "Other/a.bin", time.Time{}, writer([]byte("a"), 0), always)
	if err != nil || s2.Close() != nil {
		t.Fatal(err)
	}
	if len(readZip(t, filepath.Join(dir, "Other-0001.zip"))) != 1 || stored != "Other-0001.zip/a.bin" {
		t.Fatal("redo after crash wrong")
	}
}

func TestZipSinkRejectedCandidateNotAdded(t *testing.T) {
	dir := t.TempDir()
	s := ZipSink(destfs.New(dir), dir, store(t, dir))
	never := func(salvage.Result) bool { return false }
	stored, _, err := s.Put(context.Background(), "Other/a.bin", time.Time{}, writer([]byte("worse"), 2), never)
	if err != nil || stored != "" {
		t.Fatalf("%q %v", stored, err)
	}
	s.Close()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.zip")); len(m) != 0 {
		t.Fatalf("rejected copy written into %v", m)
	}
}

func TestCompressImageVerifiesAndRemovesRaw(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	data := append(bytes.Repeat([]byte{0}, 3<<20), randomBytes(1<<20)...)
	os.WriteFile(img, data, 0o644)
	sum, err := CompressImage(context.Background(), destfs.New(dir), img, img+".xz", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%X", sha256.Sum256(data)); sum != want {
		t.Fatalf("sum %s want %s", sum, want)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Fatal("raw image kept")
	}
	f, _ := os.Open(img + ".xz")
	defer f.Close()
	r, err := xz.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, data) {
		t.Fatal("decompressed image differs")
	}
}

func TestCompressImageKeepRaw(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	os.WriteFile(img, bytes.Repeat([]byte("x"), 1<<20), 0o644)
	if _, err := CompressImage(context.Background(), destfs.New(dir), img, img+".xz", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatal("raw image removed despite keep")
	}
}

func TestCompressImageDiskFull(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "card.img")
	os.WriteFile(img, randomBytes(2<<20), 0o644)
	full := errors.New("disk full")
	if _, err := CompressImage(context.Background(), destfs.NewQuota(destfs.New(dir), 1000, full), img, img+".xz", false, nil); !errors.Is(err, full) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatal("raw image removed after a failed compression")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("tmp left: %v", m)
	}
}

func TestEstimateWithinBounds(t *testing.T) {
	text := bytes.Repeat([]byte("abcabcabc "), 100_000)
	if r := Ratio(bytes.NewReader(text)); r <= 0 || r > 0.2 {
		t.Fatalf("text ratio %v", r)
	}
	if r := Ratio(bytes.NewReader(randomBytes(1 << 20))); r < 0.95 || r > 1.01 {
		t.Fatalf("random ratio %v", r)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	x := uint32(12345)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// A file retried after the card was re-plugged (two PARTIAL attempts) must not put the same
// entry name twice into one part: that failed verification and threw the whole part away.
func TestZipSinkSameNameTwiceKeepsPart(t *testing.T) {
	dir := t.TempDir()
	s := ZipSink(destfs.New(dir), dir, store(t, dir))
	s.Put(context.Background(), "Other/b.bin", time.Time{}, writer([]byte("other"), 0), always)
	s.Put(context.Background(), "Other/a.bin", time.Time{}, writer([]byte("first try"), 2), always)
	stored, _, err := s.Put(context.Background(), "Other/a.bin", time.Time{}, writer([]byte("second try"), 2), always)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if f := s.(*zipSink).Failed(); len(f) != 0 {
		t.Fatalf("parts discarded: %v", f)
	}
	if !s.Has(stored, int64(len("second try"))) || !s.Has("Other-0001.zip/b.bin", 5) {
		t.Fatal("entries lost")
	}
}

// An early write error with many segments in flight must not race or hang the scheduler.
func TestCompressImageEarlyErrorManySegments(t *testing.T) {
	old := segment
	segment = 64 << 10
	defer func() { segment = old }()
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		img := filepath.Join(dir, "card.img")
		os.WriteFile(img, randomBytes(2<<20), 0o644)
		full := errors.New("disk full")
		if _, err := CompressImage(context.Background(), destfs.NewQuota(destfs.New(dir), 100, full), img, img+".xz", false, nil); !errors.Is(err, full) {
			t.Fatalf("err = %v", err)
		}
	}
}
