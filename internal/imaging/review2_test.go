package imaging

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
)

// Resuming needs proof it is the same card: with no readable fingerprint sample and no matching
// file-system serial, a same-size card is refused instead of being mixed into the image.
func TestImageResumeWithoutFingerprintRefusesStranger(t *testing.T) {
	data := devData(8 * MiB)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	dev := &fakeDev{size: int64(len(data))}
	dev.r = faultreader.New(data, nil, faultreader.Options{OnRead: func(int64, int) error {
		if calls++; calls == 4 {
			cancel()
		}
		return nil
	}})
	im := newImage(t, dev)
	run(t, im, ctx)
	var meta Meta
	b, _ := os.ReadFile(im.Out + ".json")
	json.Unmarshal(b, &meta)
	meta.Fingerprint = nil // e.g. the .json was lost or the samples were never readable
	b, _ = json.Marshal(meta)
	os.WriteFile(im.Out+".json", b, 0o644)

	other := devData(8 * MiB)
	other[100] ^= 0xFF
	im2 := newImage(t, &fakeDev{r: faultreader.New(other, nil, faultreader.Options{}), size: int64(len(other))})
	im2.Out = im.Out
	if _, err := run(t, im2, context.Background()); err == nil || !strings.Contains(err.Error(), "different card") {
		t.Fatalf("err = %v", err)
	}
}
