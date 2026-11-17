package archive

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
)

type folderSink struct {
	out  destfs.FS
	root string
}

// FolderSink stores files as a normal folder tree under root: write <final>.tmp, fsync,
// re-hash it against what was read, rename into place, fsync the folder, restore the mtime.
func FolderSink(out destfs.FS, root string) Sink { return &folderSink{out: out, root: root} }

func (s *folderSink) path(dest string) string { return filepath.Join(s.root, filepath.FromSlash(dest)) }

// TmpDir holds files being written. Temp names are hashes of the destination, so they can never
// collide with a card file such as "foo.tmp" next to "foo".
const TmpDir = ".aio-tmp"

func (s *folderSink) tmpPath(dest string) string {
	h := sha1.Sum([]byte(dest))
	return filepath.Join(s.root, TmpDir, hex.EncodeToString(h[:])+".tmp")
}

func (s *folderSink) Has(dest string, size int64) bool {
	st, err := s.out.Stat(s.path(dest))
	return err == nil && st.Mode().IsRegular() && st.Size() == size
}

// Close removes what a stopped or crashed run left in TmpDir.
func (s *folderSink) Close() error {
	cleanFolderTemps(s.out, s.root)
	return nil
}

// cleanFolderTemps removes our own temp files (named by a hash) from root's TmpDir, and the
// folder itself once it is empty.
func cleanFolderTemps(out destfs.FS, root string) {
	dir := filepath.Join(root, TmpDir)
	es, err := out.ReadDir(dir)
	if err != nil {
		return // nothing there
	}
	for _, e := range es {
		n := e.Name()
		if h, ok := strings.CutSuffix(n, ".tmp"); ok && len(h) == 40 && !e.IsDir() && isHex(h) {
			out.Remove(filepath.Join(dir, n))
		}
	}
	if es, err := out.ReadDir(dir); err == nil && len(es) == 0 {
		out.Remove(dir)
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func removeIfExists(out destfs.FS, p string) error {
	if err := out.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func hashFile(p string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(p)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func (s *folderSink) Put(ctx context.Context, dest string, mtime time.Time, write func(io.Writer) (salvage.Result, error),
	accept func(salvage.Result) bool) (string, salvage.Result, error) {
	final := s.path(dest)
	if err := s.out.MkdirAll(filepath.Dir(final)); err != nil {
		return "", salvage.Result{}, err
	}
	tmp := s.tmpPath(dest)
	if err := s.out.MkdirAll(filepath.Dir(tmp)); err != nil {
		return "", salvage.Result{}, err
	}
	if err := removeIfExists(s.out, tmp); err != nil {
		return "", salvage.Result{}, err
	}
	f, err := s.out.Create(tmp)
	if err != nil {
		return "", salvage.Result{}, err
	}
	res, err := write(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		var sum [32]byte
		if sum, err = hashFile(tmp); err == nil && sum != res.Sum {
			err = ErrVerify
		}
	}
	if err == nil && !accept(res) {
		s.out.Remove(tmp)
		return "", res, nil
	}
	if err != nil {
		s.out.Remove(tmp)
		return "", res, err
	}
	stored := dest
	if res.Bad > 0 {
		stored = PartialName(dest)
	}
	target := s.path(stored)
	if err := destfs.RenameRetry(ctx, s.out, tmp, target); err != nil {
		s.out.Remove(tmp)
		return "", res, err
	}
	if res.Bad == 0 {
		removeIfExists(s.out, s.path(PartialName(dest)))
	}
	s.out.SyncDir(filepath.Dir(target))
	if !mtime.IsZero() {
		s.out.Chtimes(target, mtime)
	}
	return stored, res, nil
}
