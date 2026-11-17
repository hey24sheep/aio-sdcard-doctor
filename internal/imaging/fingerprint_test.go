package imaging

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
)

// SR3: fingerprint samples were taken only from chunks at 64 MiB boundaries; one bad sector at
// the start of the card shifted every later chunk off them, so nothing was sampled, and the next
// presence check called the card "a different card" and stopped (and a resume refused it).
func TestBadFirstSectorStillFingerprints(t *testing.T) {
	data := devData(16 * MiB)
	bad := []faultreader.Range{{Off: 0, Len: 4096}, {Off: 5 * MiB, Len: 4096}}
	dev := &fakeDev{r: faultreader.New(data, bad, faultreader.Options{}), size: int64(len(data))}
	im := newImage(t, dev)
	im.PresenceEvery = 1 // every failed read checks the card is still there
	if _, err := run(t, im, context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var meta Meta
	b, _ := os.ReadFile(im.Out + ".json")
	json.Unmarshal(b, &meta)
	if len(meta.Fingerprint) < 3 {
		t.Errorf("%d fingerprint samples, want at least 3", len(meta.Fingerprint))
	}
}

// OA3: every job that reads a card's disk takes that disk's lock too, so a card without a serial
// is locked the same way whatever identity each job worked out for it.
func TestImagingTakesTheDiskLock(t *testing.T) {
	data := devData(4 * MiB)
	dev := &fakeDev{r: faultreader.New(data, nil, faultreader.Options{}), size: int64(len(data))}
	dev.id.PhysicalID = "disk-lock-" + t.Name()
	unlock, err := job.LockSource(drives.Identity{PhysicalID: dev.id.PhysicalID}.DiskLockKey())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := run(t, newImage(t, dev), context.Background()); err == nil {
		t.Fatal("imaged a disk another job holds")
	}
}

// OR7: Set copied the whole block list on every call (1.7 ms a call with 100,000 blocks; a map
// of a badly damaged card took minutes to build). It changes the list in place.
func TestMapSetDoesNotCopyTheMap(t *testing.T) {
	m := NewMapfile(1 << 30)
	for pos := int64(0); pos < 100_000*8192; pos += 8192 {
		m.Set(pos, 4096, Finished) // 100,000 finished areas between untried ones
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for pos := int64(50_000 * 8192); pos < 51_000*8192; pos += 8192 {
		m.Set(pos+4096, 4096, BadSector)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Errorf("1,000 Set calls allocated %d MiB", grew>>20)
	}
	// and the result is right: a bad area between two finished ones
	if st := m.statusAt(50_000*8192 + 4096); st != BadSector {
		t.Errorf("status %c", st)
	}
	for i := 1; i < len(m.Blocks); i++ {
		if a, b := m.Blocks[i-1], m.Blocks[i]; a.Pos+a.Size != b.Pos || a.Status == b.Status {
			t.Fatalf("blocks %d, %d: %+v %+v", i-1, i, a, b)
		}
	}
}

// SR11: a map was refused past 1,048,576 lines, so a big card with many small bad areas could not
// resume its image. The bound follows the card's size.
func TestBigMapOfABigCardLoads(t *testing.T) {
	const blocks = 1<<20 + 10
	var b strings.Builder
	b.WriteString("0x0 + 1\n")
	for i := 0; i < blocks; i++ {
		st := '+'
		if i%2 == 1 {
			st = '-'
		}
		fmt.Fprintf(&b, "0x%X 0x200 %c\n", i*512, st)
	}
	p := filepath.Join(t.TempDir(), "card.img.map")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadMap(p, blocks*512)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blocks) != blocks {
		t.Errorf("%d blocks", len(m.Blocks))
	}
	if _, err := loadMap(p, 64<<20); err == nil {
		t.Error("a map with more blocks than its card has sectors was accepted")
	}
}
