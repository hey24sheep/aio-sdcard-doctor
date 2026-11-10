package destfs

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/ioerr"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/retry"
)

// WriteFileAtomic replaces path with data: write path.tmp, fsync, keep the old file as
// path.bak, rename into place, fsync the folder. A crash leaves either the old or the new file.
func WriteFileAtomic(f FS, path string, data []byte) error {
	tmp := path + ".tmp"
	if err := f.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	w, err := f.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		f.Remove(tmp)
		return err
	}
	if err := w.Sync(); err != nil {
		w.Close()
		f.Remove(tmp)
		return err
	}
	if err := w.Close(); err != nil {
		f.Remove(tmp)
		return err
	}
	if _, err := f.Stat(path); err == nil {
		if err := f.Remove(path + ".bak"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := f.Rename(path, path+".bak"); err != nil {
			return err
		}
	}
	if err := f.Rename(tmp, path); err != nil {
		return err
	}
	return f.SyncDir(filepath.Dir(path))
}

// RenameRetry renames a to b, retrying (5 tries) only while the file is locked by another
// program (antivirus, indexer, thumbnailer).
func RenameRetry(ctx context.Context, f FS, a, b string) error {
	return retry.Do(ctx, 5, ioerr.IsLocked, func() error { return f.Rename(a, b) })
}

// WithWatchdog calls onStuck when a single Write or Sync on f takes longer than limit (a hung
// USB disk or network share). The operation itself keeps running; the caller asks the user.
func WithWatchdog(f File, limit time.Duration, onStuck func(time.Duration)) File {
	return &watched{f, limit, onStuck}
}

type watched struct {
	File
	limit   time.Duration
	onStuck func(time.Duration)
}

func (w *watched) guard() func() bool {
	t := time.AfterFunc(w.limit, func() { w.onStuck(w.limit) })
	return t.Stop
}

func (w *watched) Write(p []byte) (int, error) {
	defer w.guard()()
	return w.File.Write(p)
}

func (w *watched) Sync() error {
	defer w.guard()()
	return w.File.Sync()
}
