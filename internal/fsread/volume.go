package fsread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// ErrMountWithOS: the volume's format has no built-in parser (APFS, HFS+, encrypted, unknown).
var ErrMountWithOS = errors.New("not readable by built-in parsers — mount with the OS")

// node is one directory entry as a driver reports it (live entries only, no "." / "..").
type node struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

// driver is what each parser adapter implements; dir is "." for the root.
type driver interface {
	list(dir string) ([]node, error)
	open(name string, n node) (io.ReaderAt, io.Closer, error)
}

// maxCached bounds the directory-listing cache (entries across all directories).
const maxCached = 500_000

// vol turns a driver into a source.Source. Every parser call is guarded: a panic inside a
// third-party library becomes an error (the worker adds a second, process-level guard).
type vol struct {
	drv    driver
	kind   string
	id     drives.Identity
	mu     sync.Mutex
	cache  map[string][]node
	cached int
}

func newVol(d driver, kind string, v Volume) *vol {
	return &vol{drv: d, kind: kind, cache: map[string][]node{},
		id: drives.Identity{FSSerial: v.Serial, Label: v.Label}}
}

func guard(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("parser-panic: %v", p)
	}
}

func (v *vol) Kind() string                   { return v.kind }
func (v *vol) Identity() drives.Identity      { return v.id }
func (v *vol) setIdentity(id drives.Identity) { v.id = id }

func (v *vol) list(dir string) (ns []node, err error) {
	defer guard(&err)
	v.mu.Lock()
	defer v.mu.Unlock()
	if ns, ok := v.cache[dir]; ok {
		return ns, nil
	}
	ns, err = v.drv.list(dir)
	if err != nil {
		return nil, err
	}
	if v.cached+len(ns) > maxCached {
		v.cache, v.cached = map[string][]node{}, 0
	}
	v.cache[dir] = ns
	v.cached += len(ns)
	return ns, nil
}

func (v *vol) find(name string) (node, error) {
	if !fs.ValidPath(name) {
		return node{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return node{name: ".", dir: true}, nil
	}
	ns, err := v.list(path.Dir(name))
	if err != nil {
		return node{}, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	base := path.Base(name)
	for _, n := range ns {
		if n.name == base {
			return n, nil
		}
	}
	for _, n := range ns {
		if strings.EqualFold(n.name, base) {
			return n, nil
		}
	}
	return node{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (v *vol) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := v.find(name)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	ns, err := v.list(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, len(ns))
	for i := range ns {
		out[i] = info(ns[i])
	}
	return out, nil
}

// Stat implements fs.StatFS.
func (v *vol) Stat(name string) (fs.FileInfo, error) {
	n, err := v.find(name)
	if err != nil {
		return nil, err
	}
	return info(n), nil
}

// OpenAt opens a file for random-access reads.
func (v *vol) OpenAt(name string) (r io.ReaderAt, size int64, c io.Closer, err error) {
	n, err := v.find(name)
	if err != nil {
		return nil, 0, nil, err
	}
	if n.dir {
		return nil, 0, nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("is a directory")}
	}
	defer guard(&err)
	r, c, err = v.drv.open(name, n)
	if err != nil {
		return nil, 0, nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return guardedReader{io.NewSectionReader(r, 0, n.size)}, n.size, c, nil
}

// Open implements fs.FS.
func (v *vol) Open(name string) (fs.File, error) {
	n, err := v.find(name)
	if err != nil {
		return nil, err
	}
	if n.dir {
		ents, err := v.ReadDir(name)
		if err != nil {
			return nil, err
		}
		return &dirFile{n: n, ents: ents}, nil
	}
	r, size, c, err := v.OpenAt(name)
	if err != nil {
		return nil, err
	}
	return &regFile{n: n, sr: io.NewSectionReader(r, 0, size), c: c}, nil
}

// guardedReader turns a parser panic during a read into an error.
type guardedReader struct{ r io.ReaderAt }

func (g guardedReader) ReadAt(p []byte, off int64) (n int, err error) {
	defer guard(&err)
	return g.r.ReadAt(p, off)
}

type info node

func (i info) Name() string               { return i.name }
func (i info) Size() int64                { return i.size }
func (i info) ModTime() time.Time         { return i.mtime }
func (i info) IsDir() bool                { return i.dir }
func (i info) Sys() any                   { return nil }
func (i info) Type() fs.FileMode          { return i.Mode().Type() }
func (i info) Info() (fs.FileInfo, error) { return i, nil }
func (i info) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type regFile struct {
	n  node
	sr *io.SectionReader
	c  io.Closer
}

func (f *regFile) Stat() (fs.FileInfo, error) { return info(f.n), nil }
func (f *regFile) Read(p []byte) (int, error) { return f.sr.Read(p) }
func (f *regFile) Close() error               { return f.c.Close() }

type dirFile struct {
	n    node
	ents []fs.DirEntry
	pos  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return info(d.n), nil }
func (d *dirFile) Read([]byte) (int, error)   { return 0, errors.New("is a directory") }
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.ents[d.pos:]
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	d.pos += len(rest)
	return rest, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// deleter is implemented by drivers that read deleted records and live cluster runs.
type deleter interface {
	deleted(ctx context.Context, emit func(source.Deleted) error) error
	live(ctx context.Context, emit func(path string, off, n int64) error) error
}

// Deleted implements source.Undeleter (errors.ErrUnsupported when the file system can't).
func (v *vol) Deleted(ctx context.Context, emit func(source.Deleted) error) (err error) {
	defer guard(&err)
	d, ok := v.drv.(deleter)
	if !ok {
		return errors.ErrUnsupported
	}
	return d.deleted(ctx, emit)
}

// LiveExtents implements source.LiveMapper (errors.ErrUnsupported when the file system can't).
func (v *vol) LiveExtents(ctx context.Context, emit func(path string, off, n int64) error) (err error) {
	defer guard(&err)
	d, ok := v.drv.(deleter)
	if !ok {
		return errors.ErrUnsupported
	}
	return d.live(ctx, emit)
}

// Open returns a read-only source for a volume found by Probe.
func Open(r io.ReaderAt, v Volume) (src source.Source, err error) {
	defer guard(&err)
	sec := io.NewSectionReader(r, v.Offset, v.Size)
	var d driver
	switch v.FS {
	case "fat12", "fat16", "fat32":
		d, err = openFAT(sec, v.Size, v.FS)
	case "exfat":
		d, err = openExFAT(sec, v.Size)
	case "ntfs":
		d, err = openNTFS(sec)
	case "ext4":
		d, err = openExt4(sec, v.Size)
	case "iso9660":
		d, err = openISO(sec, v.Size)
	default:
		return nil, ErrMountWithOS
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", v.FS, err)
	}
	return newVol(d, v.FS, v), nil
}

// SetIdentity lets the worker attach the device identity (model, serial, size) to a source.
func SetIdentity(src source.Source, id drives.Identity) {
	if v, ok := src.(*vol); ok {
		if id.FSSerial == "" {
			id.FSSerial = v.id.FSSerial
		}
		if id.Label == "" {
			id.Label = v.id.Label
		}
		v.setIdentity(id)
	}
}

const (
	reasonMountOS  = "not readable by built-in parsers — mount with the OS"
	reasonNeedsRaw = "needs raw access — choose the device (admin)"
	// NTFS deleted records and ext4/ISO file locations aren't read yet; deep scan still works.
	reasonNTFSUndelete = "not yet supported for NTFS — use Deep scan"
	reasonNoLiveMap    = "not yet supported here — live files are scanned too"
)

// Capabilities says what the built-in parsers can do on a volume (v == nil: an OS-mounted
// folder). Every disabled capability carries a reason of at most 60 characters.
func Capabilities(v *Volume, _ source.Source) []job.Capability {
	c := func(name string, ok bool, why string) job.Capability {
		if ok {
			why = ""
		}
		return job.Capability{Name: name, Enabled: ok, Reason: why}
	}
	if v == nil {
		return []job.Capability{c("Read", true, ""), c("Undelete", false, reasonNeedsRaw), c("SkipLive", false, reasonNeedsRaw)}
	}
	switch v.FS {
	case "fat12", "fat16", "fat32", "exfat":
		return []job.Capability{c("Read", true, ""), c("Undelete", true, ""), c("SkipLive", true, "")}
	case "ntfs":
		return []job.Capability{c("Read", true, ""), c("Undelete", false, reasonNTFSUndelete), c("SkipLive", false, reasonNoLiveMap)}
	case "ext4":
		return []job.Capability{c("Read", true, ""), c("Undelete", false, "ext4 wipes deleted file locations"), c("SkipLive", false, reasonNoLiveMap)}
	case "iso9660":
		return []job.Capability{c("Read", true, ""), c("Undelete", false, "read-only disc format"), c("SkipLive", false, reasonNoLiveMap)}
	}
	return []job.Capability{c("Read", false, reasonMountOS), c("Undelete", false, reasonMountOS), c("SkipLive", false, reasonMountOS)}
}
