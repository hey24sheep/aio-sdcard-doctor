package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// I4: --raw with a drive letter reads that volume's device, not the folder.
func TestRawDriveLetterMapsToVolumeDevice(t *testing.T) {
	old := lookupDrive
	defer func() { lookupDrive = old }()
	lookupDrive = func(p string) (drives.Drive, error) {
		if p == `E:\` {
			return drives.Drive{Root: `E:\`, Device: `\\.\E:`}, nil
		}
		return drives.Drive{}, errors.New("not found")
	}
	if got := rawSource(`E:\`); got != `\\.\E:` {
		t.Fatalf("got %q", got)
	}
	if got := rawSource(`\\.\PhysicalDrive2`); got != `\\.\PhysicalDrive2` {
		t.Fatalf("device path changed: %q", got)
	}
}

// I11: without a terminal, waiting for a removed card is bounded.
func TestImageWaitBoundedWithoutTTY(t *testing.T) {
	if d := waitTimeout(IO{TTY: false}); d != 10*time.Minute {
		t.Fatalf("non-TTY wait %v", d)
	}
	if d := waitTimeout(IO{TTY: true}); d != 0 {
		t.Fatalf("TTY wait %v", d)
	}
}
