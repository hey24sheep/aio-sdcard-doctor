//go:build !windows

package destfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

var lockedErrno = syscall.EBUSY

func TestChownToSudoUser(t *testing.T) {
	var got []string
	old := lchown
	defer func() { lchown = old }()
	lchown = func(p string, uid, gid int) error { got = append(got, filepath.Base(p)); return nil }
	dest, src := t.TempDir(), t.TempDir()
	f := New(dest, src)
	f.(*guarded).owner = &owner{uid: 1000, gid: 1000}
	f.MkdirAll(filepath.Join(dest, "Photos", "DCIM"))
	w, _ := f.Create(filepath.Join(dest, "Photos", "DCIM", "a.jpg"))
	w.Close()
	if len(got) < 3 || got[len(got)-1] != "a.jpg" {
		t.Fatalf("chowned %v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "Photos", "DCIM", "a.jpg")); err != nil {
		t.Fatal(err)
	}
}
