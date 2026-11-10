package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func writer(data []byte, bad int64) func(io.Writer) (salvage.Result, error) {
	return func(w io.Writer) (salvage.Result, error) {
		_, err := w.Write(data)
		return salvage.Result{Bad: bad, Sum: sha256.Sum256(data)}, err
	}
}

func always(salvage.Result) bool { return true }

func TestFolderSinkWritesVerifiedFile(t *testing.T) {
	dir := t.TempDir()
	s := FolderSink(destfs.New(dir), dir)
	mt := time.Date(2024, 1, 2, 3, 4, 6, 0, time.Local)
	stored, res, err := s.Put(context.Background(), "Photos/DCIM/a.jpg", mt, writer([]byte("jpeg!"), 0), always)
	if err != nil || stored != "Photos/DCIM/a.jpg" || res.Bad != 0 {
		t.Fatalf("%q %+v %v", stored, res, err)
	}
	p := filepath.Join(dir, "Photos", "DCIM", "a.jpg")
	if b, _ := os.ReadFile(p); string(b) != "jpeg!" {
		t.Fatalf("content %q", b)
	}
	if st, _ := os.Stat(p); !st.ModTime().Equal(mt) {
		t.Fatalf("mtime %v", st.ModTime())
	}
	if es, _ := os.ReadDir(filepath.Join(dir, TmpDir)); len(es) != 0 {
		t.Fatal("tmp left")
	}
	if !s.Has("Photos/DCIM/a.jpg", 5) || s.Has("Photos/DCIM/a.jpg", 6) {
		t.Fatal("Has wrong")
	}
}

func TestFolderSinkPartialName(t *testing.T) {
	dir := t.TempDir()
	s := FolderSink(destfs.New(dir), dir)
	stored, _, err := s.Put(context.Background(), "Videos/v.mp4", time.Time{}, writer([]byte("zz\x00\x00"), 2), always)
	if err != nil || stored != "Videos/v__PARTIAL.mp4" {
		t.Fatalf("%q %v", stored, err)
	}
	// a later clean copy replaces the partial
	stored, _, err = s.Put(context.Background(), "Videos/v.mp4", time.Time{}, writer([]byte("good"), 0), always)
	if err != nil || stored != "Videos/v.mp4" {
		t.Fatalf("%q %v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Videos", "v__PARTIAL.mp4")); !os.IsNotExist(err) {
		t.Fatal("old partial kept next to the good copy")
	}
}

func TestFolderSinkRejectedCandidateDropped(t *testing.T) {
	dir := t.TempDir()
	s := FolderSink(destfs.New(dir), dir)
	stored, _, err := s.Put(context.Background(), "a.jpg", time.Time{}, writer([]byte("worse"), 3), func(salvage.Result) bool { return false })
	if err != nil || stored != "" {
		t.Fatalf("%q %v", stored, err)
	}
	if es, _ := os.ReadDir(filepath.Join(dir, TmpDir)); len(es) != 0 {
		t.Fatalf("left %v", es)
	}
}

func TestFolderSinkHashMismatchFails(t *testing.T) {
	dir := t.TempDir()
	s := FolderSink(destfs.New(dir), dir)
	lie := func(w io.Writer) (salvage.Result, error) {
		w.Write([]byte("data"))
		return salvage.Result{Sum: sha256.Sum256([]byte("other"))}, nil
	}
	if _, _, err := s.Put(context.Background(), "a.jpg", time.Time{}, lie, always); !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v", err)
	}
	if es, _ := os.ReadDir(filepath.Join(dir, TmpDir)); len(es) != 0 {
		t.Fatalf("unverified file kept: %v", es)
	}
}

func TestFolderSinkWriteErrorRemovesTmp(t *testing.T) {
	dir := t.TempDir()
	full := errors.New("disk full")
	s := FolderSink(destfs.NewQuota(destfs.New(dir), 3, full), dir)
	if _, _, err := s.Put(context.Background(), "a.jpg", time.Time{}, writer(bytes.Repeat([]byte("x"), 10), 0), always); !errors.Is(err, full) {
		t.Fatalf("err = %v", err)
	}
	if es, _ := os.ReadDir(filepath.Join(dir, TmpDir)); len(es) != 0 {
		t.Fatal("tmp left after write error")
	}
}

// TestFolderSinkTmpNeverHitsAnotherFile: card files "foo.tmp" and "foo" share a folder.
func TestFolderSinkTmpNeverHitsAnotherFile(t *testing.T) {
	dir := t.TempDir()
	s := FolderSink(destfs.New(dir), dir)
	if _, _, err := s.Put(context.Background(), "Other/foo.tmp", time.Time{}, writer([]byte("the other file"), 0), always); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(context.Background(), "Other/foo", time.Time{}, writer([]byte("foo"), 0), always); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Other", "foo.tmp")); err != nil || string(b) != "the other file" {
		t.Fatalf("good copy of foo.tmp destroyed: %q %v", b, err)
	}
}
