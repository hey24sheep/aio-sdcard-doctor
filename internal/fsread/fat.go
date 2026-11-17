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
	"unicode/utf16"
	"unicode/utf8"

	"github.com/diskfs/go-diskfs/filesystem/fat12"
	"github.com/diskfs/go-diskfs/filesystem/fat16"
	"github.com/diskfs/go-diskfs/filesystem/fat32"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// FAT12/16/32 are read with diskfs/go-diskfs (MIT). Velocidex/go-fat was the first pick but
// failed the Task 10 gate: it treats any read-only, hidden, system or volume-label entry as a
// long-name fragment (attribute & 0x0F), so such files vanish and the next name is corrupted.

// iofsReader is what go-diskfs file systems offer for reading.
type iofsReader interface {
	ReadDir(string) ([]fs.DirEntry, error)
	Open(string) (fs.File, error)
}

// diskfsDriver adapts a go-diskfs file system (FAT, ISO9660) to driver.
//
// On FAT, go-diskfs follows cluster chains without a loop check (a looped chain allocates until
// the worker is killed), ignores read errors inside files and folders, and refuses the whole
// volume when FAT #1 is damaged. So our own parser (fatdel.go) checks every folder path first,
// lists a folder itself when its chain is damaged or go-diskfs refused the volume, and reads
// every file along its own loop-safe cluster runs.
type diskfsDriver struct {
	fsys      iofsReader  // nil: go-diskfs refused this FAT volume
	localTime bool        // FAT stores local wall-clock time; go-diskfs labels it UTC
	raw       io.ReaderAt // FAT only: deleted records and cluster runs are read from here
	kind      string
	lay       *fatLayout // FAT only: our own reading of the boot sector and the FAT
	layErr    error

	mu      sync.Mutex
	folders map[string]fatFolder // folders read by our parser, by path (files are opened in any order)
	kept    int                  // entries held in folders
}

type fatFolder struct {
	es    []fatEntry
	clean bool
	err   error // ErrPartialFolder or nil
}

// maxKeptEntries bounds the folders resolve keeps (about 100 bytes an entry).
const maxKeptEntries = 200_000

// errDirLoop: a folder record points back at one of the folders above it (damage), which would
// make the path DCIM/SUB/SUB/… endless.
var errDirLoop = errors.New("this folder points back at a folder above it (damaged folder record)")

func openFAT(r io.ReaderAt, size int64, kind string) (driver, error) {
	d := &diskfsDriver{localTime: true, raw: r, kind: kind}
	d.lay, d.layErr = readFATLayout(r, kind, size)
	if errors.Is(d.layErr, errFATTooBig) {
		return nil, d.layErr // go-diskfs would trust those sizes too
	}
	if d.layErr == nil && !d.lay.fat1 {
		return d, nil // our table needed FAT #2, which go-diskfs never reads: list it ourselves
	}
	st := newStorage(r, size)
	var f iofsReader
	var err error
	switch kind {
	case "fat12":
		f, err = fat12.Read(st, size, 0, 512)
	case "fat16":
		f, err = fat16.Read(st, size, 0, 512)
	default:
		f, err = fat32.Read(st, size, 0, 512)
	}
	if err != nil {
		if d.layErr != nil {
			return nil, err
		}
		return d, nil // e.g. "fat tables did not match": our own parser lists and reads it
	}
	d.fsys = f
	return d, nil
}

// localTime reinterprets a wall-clock time labelled UTC in the local zone, like the OS does
// when it mounts the card (R21).
func localTime(t time.Time) time.Time {
	if t.IsZero() || t.Year() < 1981 {
		return time.Time{}
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}

func (d *diskfsDriver) list(dir string) ([]node, error) {
	var own []fatEntry // our own parser's reading of dir (FAT)
	if l, err := d.layout(); err == nil {
		es, clean, err := d.resolve(l, dir)
		switch {
		case errors.Is(err, ErrPartialFolder): // go-diskfs would list it short without a word
			return liveNodes(es), err
		case err == nil && (d.fsys == nil || !clean):
			return liveNodes(es), nil
		case err == nil:
			own = es
		case d.fsys == nil || !errors.Is(err, fs.ErrNotExist):
			return nil, err
		} // else a name on the way is spelled differently by go-diskfs (it ignores a long name's checksum)
	} else if d.fsys == nil {
		return nil, err
	}
	es, err := d.fsys.ReadDir(d.path(dir))
	if err != nil {
		return nil, err
	}
	var real map[string][]fatEntry // looked up only when a name came back mangled
	var out []node
	for _, e := range es {
		if e.Name() == "." || e.Name() == ".." || e.Name() == "" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		mt := fi.ModTime()
		if d.localTime {
			mt = localTime(mt)
		}
		n := node{name: e.Name(), size: fi.Size(), mtime: mt, dir: fi.IsDir()}
		if strings.ContainsRune(n.name, utf8.RuneError) && own != nil {
			if real == nil {
				real = bySpelling(own)
			}
			// names that differ only outside the BMP share go-diskfs's spelling: take our own
			// entries in directory order, with their own sizes
			if q := real[n.name]; len(q) > 0 {
				n.name, n.size, n.dir, real[n.name] = q[0].name, q[0].size, q[0].dir, q[1:]
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func (d *diskfsDriver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	if n.size == 0 {
		return emptyReader{}, nopCloser{}, nil
	}
	if l, err := d.layout(); err == nil { // FAT: read along our own cluster runs
		es, _, err := d.resolve(l, path.Dir(name))
		if err != nil && !errors.Is(err, ErrPartialFolder) && (d.fsys == nil || !errors.Is(err, fs.ErrNotExist)) {
			return nil, nil, err
		}
		base := path.Base(name)
		if n.orig != "" { // a name the folder holds twice: open that record, not the first
			base = n.orig
		}
		if e, ok := findLiveNth(es, base, n.nth); ok && !e.dir {
			return chainReader(d.raw, l.extents(e), e.size), nopCloser{}, nil
		}
		if d.fsys == nil || diskfsName(name) != name || n.orig != "" { // go-diskfs can't tell such names apart
			return nil, nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
	}
	f, err := d.fsys.Open(d.path(name))
	if err != nil {
		return nil, nil, err
	}
	if s, ok := f.(io.ReadSeeker); ok {
		r := &seekReaderAt{rs: s}
		return r, f, nil
	}
	f.Close()
	s := &seqReaderAt{open: func() (fs.File, error) { return d.fsys.Open(d.path(name)) }}
	return s, s, nil
}

// go-diskfs decodes a FAT long name one UTF-16 unit at a time, so a character outside the BMP
// (emoji, rarer CJK; phones write them as surrogate pairs) comes back as two U+FFFD. Paths handed
// to go-diskfs are spelled its way (diskfsPath); names it lists are repaired from our own
// directory parser (realNames), which decodes UTF-16 properly.
func diskfsName(s string) string {
	astral := false
	for _, r := range s {
		astral = astral || r > 0xFFFF
	}
	if !astral {
		return s
	}
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		b.WriteRune(rune(u)) // a lone surrogate is written as U+FFFD, as go-diskfs does
	}
	return b.String()
}

// path is how go-diskfs needs p spelled: FAT only (its ISO9660 reader decodes Joliet names
// correctly).
func (d *diskfsDriver) path(p string) string {
	if d.raw == nil {
		return p
	}
	return diskfsPath(p)
}

func diskfsPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = diskfsName(s)
	}
	return strings.Join(segs, "/")
}

// bySpelling maps go-diskfs's spelling of each live name to our own entries with that spelling,
// in directory order (several when names differ only outside the BMP).
func bySpelling(es []fatEntry) map[string][]fatEntry {
	m := map[string][]fatEntry{}
	for _, e := range es {
		if !e.deleted {
			k := diskfsName(e.name)
			m[k] = append(m[k], e)
		}
	}
	return m
}

// liveNodes lists our own parser's live entries.
func liveNodes(es []fatEntry) []node {
	var out []node
	for _, e := range es {
		if e.deleted {
			continue
		}
		mt := e.mtime
		if mt.Year() < 1981 { // as localTime does for go-diskfs's times
			mt = time.Time{}
		}
		out = append(out, node{name: e.name, size: e.size, mtime: mt, dir: e.dir})
	}
	return out
}

// resolve is fatLayout.resolve with the folders it read kept: re-reading a 10,000-photo folder
// for every file opened in it would read 320 KB per file, and files copied in size order go back
// and forth between folders.
func (d *diskfsDriver) resolve(l *fatLayout, dir string) ([]fatEntry, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.folders[dir]; ok {
		return f.es, f.clean, f.err
	}
	es, clean, err := l.resolve(dir)
	if err != nil && !errors.Is(err, ErrPartialFolder) {
		return nil, false, err
	}
	if d.folders == nil || d.kept+len(es) > maxKeptEntries {
		d.folders, d.kept = map[string]fatFolder{}, 0
	}
	d.folders[dir] = fatFolder{es, clean, err}
	d.kept += len(es)
	return es, clean, err
}

// errShortChain: a live file's cluster chain ends before its size (a loop or a broken link).
var errShortChain = errors.New("the rest of this file can't be found: its cluster chain in the FAT is damaged")

// chainReader reads a live FAT file along its cluster runs. When the chain covers less than the
// file's size, reading past it fails instead of giving a silently shorter file.
func chainReader(dev io.ReaderAt, ext [][2]int64, size int64) io.ReaderAt {
	r := source.ExtentReader{Dev: dev, Extents: ext}
	var have int64
	for _, x := range ext {
		have += x[1]
	}
	if have >= size {
		return r
	}
	return shortChain{r, have}
}

type shortChain struct {
	r    io.ReaderAt
	have int64
}

func (s shortChain) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.have {
		return 0, errShortChain
	}
	n, err := s.r.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		err = errShortChain
	}
	return n, err
}

// seekReaderAt adapts a seekable file to io.ReaderAt (one reader at a time).
type seekReaderAt struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

func (s *seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s.rs, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

type emptyReader struct{}

func (emptyReader) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }

// layout is our own reading of the FAT, read once per volume; each caller gets its own record of
// the folders it has visited.
func (d *diskfsDriver) layout() (*fatLayout, error) {
	if d.lay == nil {
		if d.layErr != nil {
			return nil, d.layErr
		}
		return nil, errors.ErrUnsupported
	}
	l := *d.lay
	l.visitedDirs = map[uint32]bool{}
	return &l, nil
}

func (d *diskfsDriver) deleted(ctx context.Context, emit func(source.Deleted) error) error {
	l, err := d.layout()
	if err != nil {
		return err
	}
	return l.deleted(ctx, emit)
}

func (d *diskfsDriver) live(ctx context.Context, emit func(path string, off, n int64) error) error {
	l, err := d.layout()
	if err != nil {
		return err
	}
	return l.live(ctx, emit)
}
