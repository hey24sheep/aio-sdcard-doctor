package ioerr

import (
	"errors"
	"io/fs"
	"runtime"
	"syscall"
	"testing"
)

type pred struct {
	name string
	fn   func(error) bool
}

var preds = []pred{
	{"DiskFull", IsDiskFull}, {"TooLarge", IsTooLarge}, {"Locked", IsLocked},
	{"WriteProtected", IsWriteProtected}, {"Permission", IsPermission},
	{"Gone", IsGone}, {"UnrecognizedFS", IsUnrecognizedFS},
}

func TestPredicatesTable(t *testing.T) {
	for want, codes := range codeTable() {
		for _, c := range codes {
			err := &fs.PathError{Op: "write", Path: "x", Err: syscall.Errno(c)}
			for _, p := range preds {
				got := p.fn(err)
				if got != (p.name == want) {
					t.Errorf("errno %d: %s = %v, want %v", c, p.name, got, p.name == want)
				}
			}
		}
	}
}

func TestNilAndPlainErrors(t *testing.T) {
	for _, p := range preds {
		if p.fn(nil) || p.fn(errors.New("boom")) {
			t.Errorf("%s true for nil/plain error", p.name)
		}
	}
}

func TestWrappedErrno(t *testing.T) {
	codes := codeTable()["DiskFull"]
	err := errors.Join(errors.New("ctx"), &fs.PathError{Err: syscall.Errno(codes[0])})
	if !IsDiskFull(err) {
		t.Fatalf("wrapped disk-full not detected on %s", runtime.GOOS)
	}
}

func TestGoneWhenRootMissing(t *testing.T) {
	err := &fs.PathError{Err: syscall.Errno(rootMissingCode())}
	if !IsGoneAt(err, t.TempDir()+"/does-not-exist") {
		t.Error("missing root + not-found error should be Gone")
	}
	if IsGoneAt(err, t.TempDir()) {
		t.Error("present root + not-found error must not be Gone")
	}
	if !IsGoneAt(&fs.PathError{Err: syscall.Errno(codeTable()["Gone"][0])}, t.TempDir()) {
		t.Error("device-gone errno is Gone regardless of root")
	}
}
