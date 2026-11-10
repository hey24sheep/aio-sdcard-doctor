// Package destfs is the only way our code writes files. Every path is checked: it must be inside
// the destination folder and never inside a source (card) or a device. A violation is a bug,
// so it panics rather than returning an error that could be ignored.
package destfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// File is a file being written.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// ImageFile is a disk image being written out of order.
type ImageFile interface {
	io.WriterAt
	Sync() error
	Close() error
}

// FS is a destination folder with write guards.
type FS interface {
	MkdirAll(string) error
	Create(string) (File, error)
	OpenImage(path string, size int64, resume bool) (ImageFile, error)
	Rename(a, b string) error
	Remove(string) error
	Chtimes(string, time.Time) error
	Stat(string) (fs.FileInfo, error)
	ReadDir(string) ([]fs.DirEntry, error)
	SyncDir(string) error
}

// ErrSizeMismatch means an image being resumed is not the size of the device.
var ErrSizeMismatch = errors.New("existing image has a different size than the card")

type owner struct{ uid, gid int }

type guarded struct {
	dest  string   // canonical destination root
	srcs  []string // canonical source roots
	owner *owner   // chown created paths to this user (unix, when elevated — R12)
}

// New returns an FS that writes only inside destRoot and never inside any sourceRoot.
func New(destRoot string, sourceRoots ...string) FS {
	g := &guarded{dest: canonical(destRoot), owner: invokingUser()}
	for _, s := range sourceRoots {
		if s != "" {
			g.srcs = append(g.srcs, canonical(s))
		}
	}
	return g
}

// SetOwner makes f chown everything it creates to uid:gid (the GUI's explicit --owner, R12).
func SetOwner(f FS, uid, gid int) {
	if g, ok := f.(*guarded); ok {
		g.owner = &owner{uid, gid}
	}
}

var caseInsensitive = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// canonical is the absolute path with symlinks in its existing part resolved, lowercased where
// the file system ignores case.
func canonical(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	existing, rest := abs, ""
	for i := 0; i < 4096; i++ { // bounded walk up to the deepest existing ancestor
		if r, err := filepath.EvalSymlinks(existing); err == nil {
			abs = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	if caseInsensitive {
		abs = strings.ToLower(abs)
	}
	return abs
}

func isDevice(p string) bool {
	s := strings.ReplaceAll(p, `\`, "/")
	return strings.HasPrefix(s, "//./") || strings.HasPrefix(s, "//?/") || strings.HasPrefix(s, "/dev/")
}

func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// check panics unless p may be written; it returns p unchanged.
func (g *guarded) check(p string) string {
	if isDevice(p) {
		panic(fmt.Sprintf("destfs: write to a device path %q", p))
	}
	c := canonical(p)
	for _, s := range g.srcs {
		if under(c, s) {
			panic(fmt.Sprintf("destfs: write inside source %q: %q", s, p))
		}
	}
	rel, err := filepath.Rel(g.dest, c)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		panic(fmt.Sprintf("destfs: write outside destination %q: %q", g.dest, p))
	}
	return p
}

func (g *guarded) chown(p string) {
	if g.owner != nil {
		lchown(p, g.owner.uid, g.owner.gid)
	}
}

func (g *guarded) MkdirAll(p string) error {
	g.check(p)
	var created []string
	if g.owner != nil {
		for d := filepath.Clean(p); ; d = filepath.Dir(d) {
			if _, err := os.Lstat(d); err == nil || filepath.Dir(d) == d {
				break
			}
			created = append([]string{d}, created...)
		}
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}
	for _, d := range created {
		g.chown(d)
	}
	return nil
}

func (g *guarded) Create(p string) (File, error) {
	f, err := os.OpenFile(g.check(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	g.chown(p)
	return f, nil
}

func (g *guarded) OpenImage(p string, size int64, resume bool) (ImageFile, error) {
	g.check(p)
	if resume {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil || st.Size() != size {
			f.Close()
			if err == nil {
				err = fmt.Errorf("%w: %d bytes, card is %d", ErrSizeMismatch, st.Size(), size)
			}
			return nil, err
		}
		return f, nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	makeSparse(f)
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(p)
		return nil, err
	}
	g.chown(p)
	return f, nil
}

func (g *guarded) Rename(a, b string) error {
	return os.Rename(g.check(a), g.check(b))
}

func (g *guarded) Remove(p string) error { return os.Remove(g.check(p)) }

func (g *guarded) Chtimes(p string, t time.Time) error { return os.Chtimes(g.check(p), t, t) }

func (g *guarded) Stat(p string) (fs.FileInfo, error) { return os.Stat(p) }

func (g *guarded) ReadDir(p string) ([]fs.DirEntry, error) { return os.ReadDir(p) }

func (g *guarded) SyncDir(p string) error { return syncDir(g.check(p)) }
