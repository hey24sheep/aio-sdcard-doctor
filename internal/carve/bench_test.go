package carve

import (
	"bytes"
	"context"
	"testing"
)

// BenchmarkScan256MiB: the index pass over random data in memory (target ≥ 500 MB/s).
func BenchmarkScan256MiB(b *testing.B) {
	buf := noise(256<<20, 7)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), Options{}, 0, func(Hit) error { return nil }, nil)
	}
}
