package elevate

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestInstructionPerOS(t *testing.T) {
	args := []string{"image", "--from", "E:", "--to", `D:\My Cards\card.img`}
	if got, want := instruction("windows", args), `Run as Administrator: aio-sdcard-doctor image --from E: --to "D:\My Cards\card.img"`; got != want {
		t.Errorf("windows:\n got %s\nwant %s", got, want)
	}
	uargs := []string{"image", "--from", "/dev/sdb", "--to", "/home/u/my card's.img"}
	if got, want := instruction("linux", uargs), `sudo aio-sdcard-doctor image --from /dev/sdb --to '/home/u/my card'\''s.img'`; got != want {
		t.Errorf("unix:\n got %s\nwant %s", got, want)
	}
}

func TestWindowsQuote(t *testing.T) {
	cases := map[string]string{"a": "a", "": `""`, "a b": `"a b"`, `a"b`: `"a\"b"`, `C:\dir\`: `C:\dir\`, `C:\my dir\`: `"C:\my dir\\"`}
	for in, want := range cases {
		if got := windowsQuote(in); got != want {
			t.Errorf("%q → %s want %s", in, got, want)
		}
	}
}

type fakeUnix struct {
	tty   bool
	sudo  bool
	codes []int
	calls [][]string
}

func (f *fakeUnix) deps() unixDeps {
	return unixDeps{
		isTTY:   func() bool { return f.tty },
		hasSudo: func() bool { return f.sudo },
		exe:     func() (string, error) { return "/usr/bin/aio-sdcard-doctor", nil },
		run: func(_ context.Context, argv []string) int {
			f.calls = append(f.calls, argv)
			c := f.codes[0]
			f.codes = f.codes[1:]
			return c
		},
	}
}

func TestRelaunchNonTTYDeclines(t *testing.T) {
	f := &fakeUnix{tty: false, sudo: true}
	if err := relaunchUnix(context.Background(), []string{"image"}, f.deps()); !errors.Is(err, ErrDeclined) || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	f = &fakeUnix{tty: true, sudo: false}
	if err := relaunchUnix(context.Background(), nil, f.deps()); !errors.Is(err, ErrDeclined) {
		t.Fatalf("no sudo: %v", err)
	}
}

func TestRelaunchArgsBuilt(t *testing.T) {
	f := &fakeUnix{tty: true, sudo: true, codes: []int{0, 3}}
	err := relaunchUnix(context.Background(), []string{"rescue", "--from", "/dev/sdb"}, f.deps())
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("err = %v", err)
	}
	want := [][]string{{"sudo", "-v"}, {"sudo", "--", "/usr/bin/aio-sdcard-doctor", "rescue", "--from", "/dev/sdb"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv %v", f.calls)
	}
	f = &fakeUnix{tty: true, sudo: true, codes: []int{1}}
	if err := relaunchUnix(context.Background(), nil, f.deps()); !errors.Is(err, ErrDeclined) || len(f.calls) != 1 {
		t.Fatalf("sudo auth failure: %v %v", err, f.calls)
	}
	if got := windowsParams([]string{"image", "--to", `D:\a b\c.img`}); got != `image --to "D:\a b\c.img"` {
		t.Fatalf("windows params %s", got)
	}
}

func TestIsAdminDoesNotPanic(t *testing.T) { _ = IsAdmin() }

// Final review I6: the elevated Windows child is told it has its own window, which it keeps open.
func TestWindowsChildParamsMarkWindow(t *testing.T) {
	if got, want := childParams([]string{"image", "--from", "E:"}), "image --from E: "+WindowFlag; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
