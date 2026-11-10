package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// ErrNeedsAdmin means raw device access needs Administrator/root; the caller runs the elevate flow.
var ErrNeedsAdmin = errors.New("reading the card directly needs administrator rights")

// Source is a card's files, read-only: an OS-mounted folder or a built-in parser over a raw
// device or image. Names are fs paths ("/"-separated, relative to the card root).
type Source interface {
	fs.ReadDirFS
	fs.StatFS
	// OpenAt opens a file for random-access reads.
	OpenAt(name string) (r io.ReaderAt, size int64, c io.Closer, err error)
	// Kind is "folder", "fat12", "fat16", "fat32", "exfat", "ntfs", "ext4" or "iso9660".
	Kind() string
	Identity() drives.Identity
}

// Deleted is a deleted file found by a file-system parser.
type Deleted struct {
	Path    string
	Size    int64
	Extents [][2]int64 // byte ranges [offset, length] on the volume
	State   string     // "recoverable" | "overwritten" | "fragmented-unknown" | "metadata-damaged"
	MTime   time.Time
}

// Undeleter is implemented by sources whose file system keeps deleted entries.
type Undeleter interface {
	Deleted(ctx context.Context, emit func(Deleted) error) error
}

// LiveMapper is implemented by sources that know where live files sit, so deep scan skips them.
type LiveMapper interface {
	LiveExtents(ctx context.Context, emit func(path string, off, n int64) error) error
}
