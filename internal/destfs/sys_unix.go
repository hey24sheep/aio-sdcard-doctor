//go:build !windows

package destfs

import (
	"os"
	"strconv"
)

// syncDir fsyncs a directory so a rename into it survives a power cut.
func syncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// makeSparse is not needed: Truncate already makes a sparse file on unix file systems.
func makeSparse(*os.File) {}

// invokingUser is the user who ran sudo, so files we create while elevated stay theirs (R12).
func invokingUser() *owner {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil {
		return nil
	}
	return &owner{uid, gid}
}

var lchown = os.Lchown
