package job

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"
)

const childEnv = "AIO_JOB_STORE_CHILD"

func TestMain(m *testing.M) {
	if p := os.Getenv(childEnv); p != "" {
		storeChild(p)
		return
	}
	goleak.VerifyTestMain(m)
}

// storeChild commits one key, says "ready" and waits to be killed.
func storeChild(path string) {
	s, err := OpenStore(path)
	if err != nil {
		os.Exit(2)
	}
	if err := Put(s, "b", "committed", 42); err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("ready\n")
	select {}
}

type rec struct{ N int }

func TestStorePutGetForEach(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, k := range []string{"a", "b", "c"} {
		if err := Put(s, "x", k, rec{i}); err != nil {
			t.Fatal(err)
		}
	}
	v, ok, err := Get[rec](s, "x", "b")
	if err != nil || !ok || v.N != 1 {
		t.Fatalf("get b = %v %v %v", v, ok, err)
	}
	if _, ok, _ := Get[rec](s, "x", "zzz"); ok {
		t.Fatal("missing key found")
	}
	if _, ok, _ := Get[rec](s, "nobucket", "a"); ok {
		t.Fatal("missing bucket found")
	}
	var keys []string
	if err := ForEach(s, "x", func(k string, v rec) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys[0] != "a" || keys[2] != "c" {
		t.Fatalf("keys = %v", keys)
	}
	if err := Delete(s, "x", "a"); err != nil {
		t.Fatal(err)
	}
	if err := ClearBucket(s, "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Get[rec](s, "x", "b"); ok {
		t.Fatal("bucket not cleared")
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Put(s, "x", "k", rec{7}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, ok, _ := Get[rec](s, "x", "k"); !ok || v.N != 7 {
		t.Fatalf("after reopen: %v %v", v, ok)
	}
}

func TestStoreBusy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	_, err = OpenStore(p)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second open err = %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestStoreClosedAfterCrashReopens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "ready\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child said %q", line)
	}
	cmd.Process.Kill()
	cmd.Wait()
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, ok, _ := Get[int](s, "b", "committed"); !ok || v != 42 {
		t.Fatalf("committed key lost: %v %v", v, ok)
	}
}

func TestStoreCorruptRenamed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.db")
	junk := make([]byte, 64<<10)
	for i := range junk {
		junk[i] = byte(i * 7)
	}
	os.WriteFile(p, junk, 0o600)
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Recovered() {
		t.Fatal("Recovered() = false for a corrupt file")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "state.db.corrupt-*"))
	if len(m) != 1 {
		t.Fatalf("corrupt copy not kept: %v", m)
	}
	if err := Put(s, "x", "k", 1); err != nil {
		t.Fatal(err)
	}
}

func TestAutoPrompter(t *testing.T) {
	cases := []struct {
		yes  bool
		is   Issue
		want Choice
	}{
		{false, Issue{Severity: Warn, Default: "continue"}, "stop"},
		{true, Issue{Severity: Warn, Default: "continue"}, "continue"},
		{false, Issue{Severity: Info, Default: "resume"}, "resume"},
		{false, Issue{Severity: Warn, Default: "retry"}, "retry"},
		{true, Issue{Severity: Block, Default: "continue"}, "stop"},
		{true, Issue{Severity: Warn}, "stop"},
	}
	for i, c := range cases {
		if got := (AutoPrompter{Yes: c.yes}).Ask(context.Background(), c.is); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestThrottle(t *testing.T) {
	th := Throttle{Every: 40 * time.Millisecond}
	if !th.Ok() {
		t.Fatal("first call should pass")
	}
	if th.Ok() {
		t.Fatal("second immediate call should be throttled")
	}
	time.Sleep(50 * time.Millisecond)
	if !th.Ok() {
		t.Fatal("call after Every should pass")
	}
}

func TestInhibitReleases(t *testing.T) {
	release, err := Inhibit("test")
	if err != nil {
		t.Skipf("inhibit unavailable here: %v", err)
	}
	release()
	release() // idempotent
}

func TestInhibitUnavailableWarnsOnce(t *testing.T) {
	old := inhibitImpl
	defer func() { inhibitImpl = old; inhibitWarned.Store(false) }()
	inhibitWarned.Store(false)
	inhibitImpl = func(string) (func(), error) { return nil, errors.New("no logind") }
	r1, err1 := Inhibit("a")
	r2, err2 := Inhibit("b")
	if err1 == nil || err2 != nil {
		t.Fatalf("errs = %v, %v; want first only", err1, err2)
	}
	r1()
	r2()
}

func TestSecondRunOnSameCardRefused(t *testing.T) {
	key := "test-" + filepath.Base(t.TempDir())
	unlock, err := LockSource(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockSource(key); !errors.Is(err, ErrSourceBusy) {
		unlock()
		t.Fatalf("second lock err = %v, want ErrSourceBusy", err)
	}
	unlock()
	unlock2, err := LockSource(key)
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	unlock2()
}

// TestSeverityJSON: front ends get severities as words, the same as String().
func TestSeverityJSON(t *testing.T) {
	b, _ := json.Marshal([]Severity{Info, Warn, Block})
	if string(b) != `["info","warning","blocked"]` {
		t.Fatalf("%s", b)
	}
	var back []Severity
	if err := json.Unmarshal(b, &back); err != nil || back[1] != Warn || back[2] != Block {
		t.Fatalf("%v %v", back, err)
	}
}
