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

// ErrPartialFolder: part of a folder's records could not be read (a bad sector, or blank where an
// image holds an area the card couldn't give). ReadDir returns it (in an *fs.PathError naming the
// folder) together with every entry that could be read, so fs.WalkDir reports the folder and
// still walks those entries; scan records the folder as not fully listed.
var ErrPartialFolder = errors.New("part of this folder could not be read, so some of its files may be missing from this list")

// node is one directory entry as a driver reports it (live entries only, no "." / "..").
type node struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
	orig  string // a name the folder holds twice: the record's real name (name is made unique)
	nth   int    // and which of the records with that name this is (0 = the first)
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
	drv     driver
	kind    string
	id      drives.Identity
	mu      sync.Mutex
	cache   map[string][]node
	partial map[string]error // folders listed only in part (ErrPartialFolder)
	cached  int
}

func newVol(d driver, kind string, v Volume) *vol {
	return &vol{drv: d, kind: kind, cache: map[string][]node{}, partial: map[string]error{},
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

// list lists dir. With ErrPartialFolder (part of the folder couldn't be read) the entries that
// could be read come with it, and both are kept.
func (v *vol) list(dir string) (ns []node, err error) {
	defer guard(&err)
	v.mu.Lock()
	defer v.mu.Unlock()
	if ns, ok := v.cache[dir]; ok {
		return ns, v.partial[dir]
	}
	if strings.Count(dir, "/") >= maxDepth { // only a folder pointing back up the tree goes this deep
		return nil, errTooDeep
	}
	ns, err = v.drv.list(dir)
	if err != nil && !errors.Is(err, ErrPartialFolder) {
		return nil, err
	}
	sameNames(ns)
	if v.cached+len(ns) > maxCached {
		v.cache, v.partial, v.cached = map[string][]node{}, map[string]error{}, 0
	}
	v.cache[dir] = ns
	if err != nil {
		v.partial[dir] = err
	}
	v.cached += len(ns)
	return ns, err
}

// sameNames gives every record after the first with a name the folder already holds (damage
// chkdsk reports as duplicate names) a name of its own, "A (2).TXT", and keeps the real name and
// which record it is, so each is listed once and opened as itself.
func sameNames(ns []node) {
	names := make(map[string]int, len(ns)) // name → records seen with it so far
	for _, n := range ns {
		names[n.name] = 0
	}
	for i := range ns {
		k := names[ns[i].name]
		names[ns[i].name] = k + 1
		if k == 0 {
			continue
		}
		orig, ext := ns[i].name, path.Ext(ns[i].name)
		if ns[i].dir {
			ext = ""
		}
		for j := k + 1; ; j++ {
			name := fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(orig, ext), j, ext)
			if _, taken := names[name]; !taken {
				ns[i].name, ns[i].orig, ns[i].nth = name, orig, k
				names[name] = 1
				break
			}
		}
	}
}

func (v *vol) find(name string) (node, error) {
	if !fs.ValidPath(name) {
		return node{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return node{name: ".", dir: true}, nil
	}
	ns, err := v.list(path.Dir(name))
	if err != nil && !errors.Is(err, ErrPartialFolder) { // a name read from a partial folder is found
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
	if err != nil && !errors.Is(err, ErrPartialFolder) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, len(ns))
	for i := range ns {
		out[i] = info(ns[i])
	}
	if err != nil { // the readable entries and the error: fs.WalkDir reports it, then walks them
		return out, &fs.PathError{Op: "readdir", Path: name, Err: err}
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
	var sec io.ReaderAt = io.NewSectionReader(r, v.Offset, v.Size)
	if v.FS == "fat32" || v.FS == "exfat" {
		if fsBoot(readAt(sec, 0, sector)) != v.FS { // the boot sector is unreadable or wiped
			if c, k := backupBoot(sec); k == v.FS {
				sec = bootCopy{sec, c}
			}
		}
	}
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

// bootCopy reads r with the backup copy b of the boot sectors in place of the volume's first
// sectors (FAT32 and exFAT keep one; the parsers read only the first).
type bootCopy struct {
	r io.ReaderAt
	b []byte
}

func (c bootCopy) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(c.b)) {
		return c.r.ReadAt(p, off)
	}
	n := copy(p, c.b[off:])
	if n == len(p) {
		return n, nil
	}
	m, err := c.r.ReadAt(p[n:], int64(len(c.b)))
	return n + m, err
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
