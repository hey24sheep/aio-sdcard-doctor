//go:build !windows

package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockDir is the per-user runtime directory for card locks (never on the card).
func lockDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "aio-sdcard-doctor")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("aio-sdcard-doctor-%d", os.Getuid()))
}

// lockSourceOS takes an flock on a per-card lock file; the kernel drops it if we crash.
func lockSourceOS(hash string) (func(), error) {
	dir := lockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, hash+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrSourceBusy
		}
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
