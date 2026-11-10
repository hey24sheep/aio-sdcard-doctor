// Package archive is where rescued files end up: a plain folder tree (FolderSink) or, with the
// Compress option, ZIP parts at maximum compression. Every file is verified after writing.
package archive

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
)

// ErrVerify means the written copy did not hash to what was read from the card.
var ErrVerify = errors.New("the copy on the destination doesn't match what was read")

// Sink stores finished files.
type Sink interface {
	// Put writes one file. dest is the planned "/"-separated path; write copies the data and
	// returns the salvage result; accept decides (with the result) whether to keep the copy,
	// e.g. never replace a good copy with a worse one. stored is "" when not kept.
	Put(ctx context.Context, dest string, mtime time.Time, write func(io.Writer) (salvage.Result, error),
		accept func(salvage.Result) bool) (stored string, res salvage.Result, err error)
	// Has reports whether dest is already stored with this size.
	Has(dest string, size int64) bool
	Close() error
}

// PartialName is dest with __PARTIAL before the extension.
func PartialName(dest string) string {
	ext := extOf(dest)
	return dest[:len(dest)-len(ext)] + "__PARTIAL" + ext
}

func extOf(p string) string {
	for i := len(p) - 1; i >= 0 && p[i] != '/'; i-- {
		if p[i] == '.' {
			return p[i:]
		}
	}
	return ""
}
