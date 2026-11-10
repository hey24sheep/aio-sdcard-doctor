package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A lock taken by an elevated run is not openable by a normal one ("Access is denied"); that is
// still "the card is busy", not an error the user can't act on.
func TestLockHeldByElevatedRunIsBusy(t *testing.T) {
	key := "elevated-" + filepath.Base(t.TempDir())
	sum := sha256.Sum256([]byte(key))
	name, _ := windows.UTF16PtrFromString(`Local\aio-sdcard-doctor-` + hex.EncodeToString(sum[:8]))
	sd, err := windows.SecurityDescriptorFromString("D:(D;;GA;;;WD)") // deny everyone everything
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{Length: 24, SecurityDescriptor: sd}
	h, err := windows.CreateMutex(sa, false, name)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if _, err := LockSource(key); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("err = %v, want ErrSourceBusy", err)
	}
}
