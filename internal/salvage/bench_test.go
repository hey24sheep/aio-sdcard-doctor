package salvage

import (
	"context"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage/faultreader"
)

func BenchmarkCopy(b *testing.B) {
	d := data(64 * MiB)
	r := faultreader.New(d, nil, faultreader.Options{})
	c := NewCopier()
	b.SetBytes(int64(len(d)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Copy(context.Background(), r, int64(len(d)), discard{})
	}
}
