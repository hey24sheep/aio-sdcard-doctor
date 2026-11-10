package worker

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

const (
	envWorker = "AIO_TEST_WORKER" // this test binary acts as the worker
	envParent = "AIO_TEST_PARENT" // this test binary acts as a parent that gets killed
)

func TestMain(m *testing.M) {
	if os.Getenv(envWorker) == "1" {
		os.Exit(Main())
	}
	if p := os.Getenv(envParent); p != "" {
		parentThenDie(p)
		return
	}
	// The worker is this test binary re-executed with test hooks enabled.
	Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1", hooksEnv+"=1")
		return c
	}
	goleak.VerifyTestMain(m)
}

// parentThenDie spawns a worker, prints its pid and blocks until killed.
func parentThenDie(dir string) {
	Command = func() *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), envWorker+"=1", envParent+"=")
		return c
	}
	c, err := Spawn(Spec{From: dir}, time.Second)
	if err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString(strconv.Itoa(c.Pid()) + "\n")
	select {}
}

func folder(t *testing.T) string {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "DCIM", "Camera"), 0o755)
	os.WriteFile(filepath.Join(d, "DCIM", "Camera", "a.jpg"), bytes.Repeat([]byte("jpeg"), 700_000), 0o644)
	os.WriteFile(filepath.Join(d, "notes.txt"), []byte("hello"), 0o644)
	return d
}

func spawn(t *testing.T, spec Spec, stall time.Duration) *Client {
	t.Helper()
	c, err := Spawn(spec, stall)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestWorkerReadsFolder(t *testing.T) {
	d := folder(t)
	c := spawn(t, Spec{From: d}, 5*time.Second)
	if c.Kind() != "folder" {
		t.Fatalf("kind %s", c.Kind())
	}
	es, err := c.ReadDir("DCIM/Camera")
	if err != nil || len(es) != 1 || es[0].Name() != "a.jpg" {
		t.Fatalf("%v %v", es, err)
	}
	r, size, cl, err := c.OpenAt("DCIM/Camera/a.jpg")
	if err != nil || size != 2_800_000 {
		t.Fatal(err, size)
	}
	got := make([]byte, size)
	if n, err := r.ReadAt(got, 0); n != int(size) || (err != nil && err != io.EOF) {
		t.Fatal(n, err)
	}
	cl.Close()
	want, _ := os.ReadFile(filepath.Join(d, "DCIM", "Camera", "a.jpg"))
	if !bytes.Equal(got, want) {
		t.Fatal("bytes differ")
	}
	if _, err := c.Stat("missing.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if b, err := fs.ReadFile(c, "notes.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("fs.ReadFile %q %v", b, err)
	}
}

func TestWorkerReadsImage(t *testing.T) {
	f, _ := os.Open(filepath.Join("..", "fsread", "testdata", "ntfs.img.gz"))
	defer f.Close()
	z, _ := gzip.NewReader(f)
	img := filepath.Join(t.TempDir(), "ntfs.img")
	b, _ := io.ReadAll(z)
	os.WriteFile(img, b, 0o644)
	c := spawn(t, Spec{From: img, Raw: true}, 5*time.Second)
	if c.Kind() != "ntfs" || len(c.Volumes()) != 1 {
		t.Fatalf("kind %s vols %v", c.Kind(), c.Volumes())
	}
	if _, err := c.ReadDir("Folder A/Folder B"); err != nil {
		t.Fatal(err)
	}
	r, size, cl, err := c.OpenDevice()
	if err != nil || size != int64(len(b)) {
		t.Fatal(err, size)
	}
	head := make([]byte, 512)
	r.ReadAt(head, 0)
	cl.Close()
	if !bytes.Equal(head, b[:512]) {
		t.Fatal("device bytes differ")
	}
}

func TestWorkerParserHang(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 300*time.Millisecond)
	start := time.Now()
	err := c.hook("Hang")
	if !errors.Is(err, source.ErrStalled) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond+500*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	if _, err := c.ReadDir("."); err != nil {
		t.Fatalf("after respawn: %v", err)
	}
}

func TestWorkerParserPanic(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 5*time.Second)
	if err := c.hook("Panic"); err == nil || !strings.Contains(err.Error(), "parser-panic") {
		t.Fatalf("handler panic: %v", err)
	}
	if err := c.hook("Crash"); !errors.Is(err, source.ErrWorkerCrashed) {
		t.Fatalf("crash: %v", err)
	}
	if _, err := c.ReadDir("."); err != nil {
		t.Fatalf("after respawn: %v", err)
	}
}

func TestWorkerRestartBudget(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 100*time.Millisecond)
	for i := 0; i < MaxRestarts; i++ {
		if err := c.hook("Crash"); !errors.Is(err, source.ErrWorkerCrashed) {
			t.Fatalf("crash %d: %v", i, err)
		}
	}
	if err := c.hook("Crash"); !errors.Is(err, source.ErrTooManyRestarts) {
		t.Fatalf("6th failure: %v", err)
	}
	c.ResetBudget()
	if _, err := c.ReadDir("."); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

func TestRestartBudgetWindow(t *testing.T) {
	now := time.Unix(0, 0)
	b := budget{now: func() time.Time { return now }}
	for i := 0; i < MaxRestarts; i++ {
		if !b.take() {
			t.Fatal("budget spent early")
		}
	}
	if b.take() {
		t.Fatal("6th restart in the window allowed")
	}
	now = now.Add(RestartWindow + time.Second)
	if !b.take() {
		t.Fatal("budget not refilled after the window")
	}
}

func TestWorkerSurvivesLibraryPrintf(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 5*time.Second)
	if err := c.hook("Printf"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadDir("DCIM"); err != nil {
		t.Fatalf("RPC broken by library output: %v", err)
	}
}

func TestWorkerStderrDrained(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 5*time.Second)
	start := time.Now()
	if err := c.hook("Stderr"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("1 MiB of stderr stalled the worker")
	}
	if !strings.Contains(c.StderrTail(), "noise") {
		t.Fatal("stderr not captured")
	}
}

func TestWorkerMemoryCapKills(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 2 GiB")
	}
	c := spawn(t, Spec{From: folder(t)}, 30*time.Second)
	if err := c.hook("Alloc"); !errors.Is(err, source.ErrWorkerCrashed) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoChildLeftOnClose(t *testing.T) {
	c, err := Spawn(Spec{From: folder(t)}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pid := c.Pid()
	c.Close()
	if alive(pid) {
		t.Fatalf("worker %d still running after Close", pid)
	}
}

func TestWorkerExitsWhenParentDies(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envParent+"="+folder(t))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, 32)
	n, _ := out.Read(line)
	pid, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("parent said %q", line[:n])
	}
	cmd.Process.Kill()
	cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("orphaned worker %d still running", pid)
	}
}

// TestWorkerReadOnlyHandles: the worker's RPC surface has no method that writes.
func TestWorkerReadOnlyHandles(t *testing.T) {
	allowed := map[string]bool{"Open": true, "ReadDir": true, "Stat": true, "OpenFile": true, "ReadAt": true,
		"Close": true, "OpenDevice": true, "Ping": true, "Hook": true,
		"Deleted": true, "LiveExtents": true, "Validate": true} // listings of deleted records and live runs: read-only (R17)
	tp := reflect.TypeOf(&Service{})
	for i := 0; i < tp.NumMethod(); i++ {
		if name := tp.Method(i).Name; !allowed[name] {
			t.Errorf("unexpected worker method %s", name)
		}
	}
}

func TestGarbageNoPanicThroughWorker(t *testing.T) {
	img := filepath.Join(t.TempDir(), "junk.img")
	b := make([]byte, 8<<20)
	for i := range b {
		b[i] = byte(i*131 + i>>9)
	}
	copy(b[3:], "NTFS    ")
	b[510], b[511] = 0x55, 0xAA
	os.WriteFile(img, b, 0o644)
	c, err := Spawn(Spec{From: img, Raw: true}, 3*time.Second)
	if err == nil {
		c.ReadDir(".")
		c.Close()
	}
}

// TestRespawnWaitsForReaper: a new worker starts only after the killed one is gone (R10).
func TestRespawnWaitsForReaper(t *testing.T) {
	c := spawn(t, Spec{From: folder(t)}, 200*time.Millisecond)
	old := c.Pid()
	if err := c.hook("Hang"); !errors.Is(err, source.ErrStalled) {
		t.Fatal(err)
	}
	if _, err := c.ReadDir("."); err != nil {
		t.Fatal(err)
	}
	if alive(old) {
		t.Fatalf("old worker %d still alive after respawn", old)
	}
	if c.Pid() == old || c.Pid() == 0 {
		t.Fatal("no new worker")
	}
}
