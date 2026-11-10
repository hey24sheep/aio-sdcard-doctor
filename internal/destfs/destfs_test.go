package destfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func setup(t *testing.T) (dest, src string, f FS) {
	t.Helper()
	root := t.TempDir()
	dest, src = filepath.Join(root, "dest"), filepath.Join(root, "src")
	os.MkdirAll(dest, 0o755)
	os.MkdirAll(src, 0o755)
	return dest, src, New(dest, src)
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", what)
		}
	}()
	fn()
}

func everyMethod(f FS, p string) map[string]func() {
	return map[string]func(){
		"MkdirAll":  func() { f.MkdirAll(p) },
		"Create":    func() { f.Create(p) },
		"OpenImage": func() { f.OpenImage(p, 10, false) },
		"RenameSrc": func() { f.Rename(p, p+"x") },
		"RenameDst": func() { f.Rename(p+"x", p) },
		"Remove":    func() { f.Remove(p) },
		"Chtimes":   func() { f.Chtimes(p, time.Now()) },
		"SyncDir":   func() { f.SyncDir(p) },
	}
}

func TestGuardPanicsInsideSource(t *testing.T) {
	_, src, f := setup(t)
	bad := []string{src, filepath.Join(src, "a.jpg"), filepath.Join(src, "sub", "x")}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		bad = append(bad, strings.ToUpper(src), filepath.Join(strings.ToLower(src), "A.JPG"))
	}
	for _, p := range bad {
		for name, fn := range everyMethod(f, p) {
			mustPanic(t, name+" "+p, fn)
		}
	}
}

func TestGuardPanicsOutsideDest(t *testing.T) {
	dest, _, f := setup(t)
	for _, p := range []string{filepath.Dir(dest), filepath.Join(dest, "..", "elsewhere"), filepath.Join(dest+"x", "f")} {
		for name, fn := range everyMethod(f, p) {
			mustPanic(t, name+" "+p, fn)
		}
	}
}

func TestGuardPanicsOnDevices(t *testing.T) {
	_, _, f := setup(t)
	for _, p := range []string{`\\.\E:`, `\\.\PhysicalDrive1`, "/dev/sdb", "/dev/rdisk2"} {
		mustPanic(t, "Create "+p, func() { f.Create(p) })
	}
}

func TestPrefixIsNotParent(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "x")
	dest := filepath.Join(root, "xy")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dest, 0o755)
	f := New(dest, src)
	w, err := f.Create(filepath.Join(dest, "ok.txt"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
}

func TestGuardFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	src, dest := filepath.Join(root, "src"), filepath.Join(root, "dest")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dest, 0o755)
	link := filepath.Join(dest, "link")
	if err := os.Symlink(src, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := New(dest, src)
	mustPanic(t, "create through symlink", func() { f.Create(filepath.Join(link, "a.jpg")) })
}

func TestCreateIsExclusive(t *testing.T) {
	dest, _, f := setup(t)
	p := filepath.Join(dest, "a.bin")
	w, err := f.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	w.Close()
	if _, err := f.Create(p); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create err = %v, want ErrExist", err)
	}
}

func TestWriteFileAtomicKeepsBak(t *testing.T) {
	dest, _, f := setup(t)
	p := filepath.Join(dest, "manifest.json")
	if err := WriteFileAtomic(f, p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(f, p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
	if b, _ := os.ReadFile(p + ".bak"); string(b) != "one" {
		t.Fatalf(".bak %q", b)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp left behind")
	}
}

type flakyRename struct {
	FS
	fails, calls int
}

func (f *flakyRename) Rename(a, b string) error {
	f.calls++
	if f.calls <= f.fails {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: lockedErrno}
	}
	return f.FS.Rename(a, b)
}

func TestRenameRetry(t *testing.T) {
	dest, _, f := setup(t)
	a, b := filepath.Join(dest, "a"), filepath.Join(dest, "b")
	w, _ := f.Create(a)
	w.Close()
	fl := &flakyRename{FS: f, fails: 2}
	if err := RenameRetry(context.Background(), fl, a, b); err != nil || fl.calls != 3 {
		t.Fatalf("err=%v calls=%d", err, fl.calls)
	}
	fl = &flakyRename{FS: f, fails: 99}
	if err := RenameRetry(context.Background(), fl, b, a); err == nil || fl.calls != 5 {
		t.Fatalf("err=%v calls=%d", err, fl.calls)
	}
	fl = &flakyRename{FS: f, fails: 0}
	if err := RenameRetry(context.Background(), fl, filepath.Join(dest, "missing"), a); err == nil || fl.calls != 1 {
		t.Fatalf("non-lock error retried: calls=%d", fl.calls)
	}
}

func TestOpenImageNewAndResume(t *testing.T) {
	dest, _, f := setup(t)
	p := filepath.Join(dest, "card.img")
	img, err := f.OpenImage(p, 1<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	img.WriteAt([]byte("tail"), 1<<20-4)
	img.Close()
	if st, _ := os.Stat(p); st.Size() != 1<<20 {
		t.Fatalf("size %d", st.Size())
	}
	if _, err := f.OpenImage(p, 1<<20, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("new over existing: %v", err)
	}
	img, err = f.OpenImage(p, 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	img.Close()
}

func TestOpenImageResumeSizeMismatch(t *testing.T) {
	dest, _, f := setup(t)
	p := filepath.Join(dest, "card.img")
	img, _ := f.OpenImage(p, 4096, false)
	img.Close()
	if _, err := f.OpenImage(p, 8192, true); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestQuota(t *testing.T) {
	dest, _, f := setup(t)
	full := errors.New("disk full")
	q := NewQuota(f, 100, full)
	w, err := q.Create(filepath.Join(dest, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write(make([]byte, 60)); n != 60 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write(make([]byte, 60)); n != 40 || !errors.Is(err, full) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	w.Close()
	img, _ := q.OpenImage(filepath.Join(dest, "i"), 1000, false)
	if _, err := img.WriteAt(make([]byte, 10), 0); !errors.Is(err, full) {
		t.Fatalf("image over quota: %v", err)
	}
	img.Close()
}

type slowFile struct{ d time.Duration }

func (s slowFile) Write(p []byte) (int, error) { time.Sleep(s.d); return len(p), nil }
func (s slowFile) Sync() error                 { time.Sleep(s.d); return nil }
func (s slowFile) Close() error                { return nil }

func TestDestWriteWatchdog(t *testing.T) {
	stuck := make(chan time.Duration, 4)
	w := WithWatchdog(slowFile{80 * time.Millisecond}, 20*time.Millisecond, func(d time.Duration) { stuck <- d })
	w.Write([]byte("x"))
	w.Sync()
	w.Close()
	if len(stuck) != 2 {
		t.Fatalf("watchdog fired %d times, want 2", len(stuck))
	}
	fast := WithWatchdog(slowFile{0}, time.Second, func(time.Duration) { t.Error("fired on a fast write") })
	fast.Write([]byte("x"))
	fast.Close()
}

var _ io.Writer = File(nil)
var _ = syscall.Errno(0)

func TestFree(t *testing.T) {
	n, err := Free(t.TempDir())
	if err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
}
