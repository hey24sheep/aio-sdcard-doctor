package rescue

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
)

// twoPartitionImage is an MBR disk image with two FAT32 partitions (a camera card, then a GoPro).
func twoPartitionImage(t *testing.T) string {
	t.Helper()
	const part = 40 << 20
	a, _ := os.ReadFile(testutil.FAT32Image(t, testutil.CameraCard(), part))
	b, _ := os.ReadFile(testutil.FAT32Image(t, testutil.GoProCard(), part))
	img := make([]byte, 1<<20, 1<<20+2*part)
	for i, lba := range []uint32{2048, 2048 + part/512} {
		e := img[446+16*i:]
		e[4] = 0x0C
		binary.LittleEndian.PutUint32(e[8:], lba)
		binary.LittleEndian.PutUint32(e[12:], part/512)
	}
	img[510], img[511] = 0x55, 0xAA
	img = append(append(img, a...), b...)
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// SA-1 follow-up: a card with several partitions copies one of them; the GUI and TUI said
// nothing because only the CLI printed a note. The plan now says it, unless the user picked one.
func TestPlanSaysWhenACardHasMorePartitions(t *testing.T) {
	img := twoPartitionImage(t)
	note := func(r *Rescue) string {
		t.Helper()
		p, err := r.Plan(context.Background(), job.Env{})
		defer r.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, is := range p.Issues {
			if is.Kind == "more-partitions" {
				if is.Severity != job.Info {
					t.Errorf("severity %v", is.Severity)
				}
				return is.Title + " — " + is.Detail
			}
		}
		return ""
	}
	mk := func() *Rescue {
		dst := filepath.Join(t.TempDir(), "out")
		return &Rescue{From: img, Raw: true, Dest: dst, StallTimeout: 10 * time.Second, LookupDrive: testDrives(img, dst)}
	}
	if s := note(mk()); !strings.Contains(s, "2 partitions") || !strings.Contains(s, "only partition 1 is copied") || !strings.Contains(s, "--volume") ||
		!strings.Contains(s, "into a new folder") { // OC8: the same folder is refused as a different card
		t.Fatalf("first partition: %q", s)
	}
	r := mk()
	r.Volume = 1
	if s := note(r); !strings.Contains(s, "only partition 2 is copied") {
		t.Fatalf("second partition: %q", s)
	}
	r = mk()
	r.Volume, r.VolumePicked = 1, true
	if s := note(r); s != "" {
		t.Fatalf("picked with --volume, yet: %q", s)
	}
}
