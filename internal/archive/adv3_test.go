package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
)

// syncFailsOnce fails the first fsync of a ZIP part (the disk filled up as it was closed).
type syncFailsOnce struct {
	destfs.FS
	done atomic.Bool
}

type syncFailFile struct {
	destfs.File
	fail bool
}

func (f syncFailFile) Sync() error {
	if f.fail {
		return errors.New("There is not enough space on the disk.")
	}
	return f.File.Sync()
}

func (s *syncFailsOnce) Create(p string) (destfs.File, error) {
	f, err := s.FS.Create(p)
	if err != nil {
		return f, err
	}
	return syncFailFile{f, strings.HasSuffix(p, ".zip.tmp") && s.done.CompareAndSwap(false, true)}, nil
}

// OC3: a part that failed as it was closed was deleted and its number used again, so the same
// file put again got the very name listed as failed — and its good copy was then reported FAILED.
func TestZipPartNumberNeverReusedInRun(t *testing.T) {
	old := MaxPartBytes
	MaxPartBytes = 1 // every file closes its part
	defer func() { MaxPartBytes = old }()
	dir := t.TempDir()
	s := ZipSink(&syncFailsOnce{FS: destfs.New(dir)}, dir, store(t, dir))
	first, _, err := s.Put(context.Background(), "Photos/a.jpg", time.Time{}, writer(randomBytes(5000), 0), always)
	if err == nil {
		t.Fatal("the failed part wasn't reported")
	}
	again, _, err := s.Put(context.Background(), "Photos/a.jpg", time.Time{}, writer(randomBytes(5000), 0), always)
	if err != nil {
		t.Fatal(err)
	}
	if again == first {
		t.Fatalf("the retried file got the failed part's name %s", again)
	}
	for _, f := range s.(*zipSink).Failed() {
		if f == again {
			t.Fatalf("the good copy %s is listed as failed", again)
		}
	}
}

type renameFailsOnce struct {
	destfs.FS
	done atomic.Bool
}

func (f *renameFailsOnce) Rename(a, b string) error {
	if strings.HasSuffix(a, ".zip.tmp") && f.done.CompareAndSwap(false, true) {
		return errors.New("Access is denied (simulated antivirus)")
	}
	return f.FS.Rename(a, b)
}

// OC4: one part that couldn't be renamed into place left its .zip.tmp, and every later part of
// that category then failed with "file exists".
func TestZipRenameFailureDoesNotBlockLaterParts(t *testing.T) {
	old := MaxPartBytes
	MaxPartBytes = 1
	defer func() { MaxPartBytes = old }()
	dir := t.TempDir()
	s := ZipSink(&renameFailsOnce{FS: destfs.New(dir)}, dir, store(t, dir))
	ctx := context.Background()
	if _, _, err := s.Put(ctx, "Photos/a.jpg", time.Time{}, writer(randomBytes(5000), 0), always); err == nil {
		t.Fatal("the failed rename wasn't reported")
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.zip.tmp")); len(tmps) != 0 {
		t.Fatalf("left behind: %v", tmps)
	}
	for _, n := range []string{"Photos/b.jpg", "Photos/c.jpg"} {
		if _, _, err := s.Put(ctx, n, time.Time{}, writer(randomBytes(5000), 0), always); err != nil {
			t.Fatalf("%s after one failed rename: %v", n, err)
		}
	}
}

// A part number whose .zip.tmp is still there (it couldn't be removed) is skipped (OC4).
func TestZipNextNumberSkipsLeftoverTmp(t *testing.T) {
	dir := t.TempDir()
	s := &zipSink{out: destfs.New(dir), root: dir}
	os.WriteFile(filepath.Join(dir, "Photos-0003.zip.tmp"), []byte("x"), 0o644)
	if n := s.nextNumber("Photos"); n != 4 {
		t.Fatalf("next %d", n)
	}
}

// SC4: a run that crashed in one mode left temp files the other mode never removed. CleanTemps
// removes both sets — only our own temp names.
func TestCleanTempsRemovesBothModesOnly(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, TmpDir), 0o755)
	ours := []string{filepath.Join(dir, TmpDir, strings.Repeat("ab", 20)+".tmp"), filepath.Join(dir, "Photos-0002.zip.tmp"), filepath.Join(dir, spoolName)}
	theirs := []string{filepath.Join(dir, "notes.tmp"), filepath.Join(dir, "holiday.zip.tmp"), filepath.Join(dir, TmpDir, "keep.tmp")}
	for _, p := range append(append([]string{}, ours...), theirs...) {
		os.WriteFile(p, []byte("x"), 0o644)
	}
	CleanTemps(destfs.New(dir), dir)
	for _, p := range ours {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("left: %s", p)
		}
	}
	for _, p := range theirs {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("removed someone else's file: %s", p)
		}
	}
}
