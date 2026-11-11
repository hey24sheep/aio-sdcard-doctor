package fsread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
type diskfsDriver struct {
	fsys      iofsReader
	localTime bool        // FAT stores local wall-clock time; go-diskfs labels it UTC
	raw       io.ReaderAt // FAT only: deleted records and cluster runs are read from here
	kind      string
}

func openFAT(r io.ReaderAt, size int64, kind string) (driver, error) {
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
		return nil, err
	}
	return &diskfsDriver{fsys: f, localTime: true, raw: r, kind: kind}, nil
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
		if strings.ContainsRune(n.name, utf8.RuneError) && d.raw != nil {
			if real == nil {
				real = d.realEntries(dir)
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
	if d.raw != nil && diskfsName(name) != name { // go-diskfs can't tell such names apart: read the chain
		l, err := d.layout()
		if err != nil {
			return nil, nil, err
		}
		e, ok := l.lookup(name)
		if !ok || e.dir {
			return nil, nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		return source.ExtentReader{Dev: d.raw, Extents: l.extents(e)}, nopCloser{}, nil
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

// realEntries maps go-diskfs's spelling of each live name in dir to our own entries with that
// spelling, in directory order (several when names differ only outside the BMP).
func (d *diskfsDriver) realEntries(dir string) map[string][]fatEntry {
	l, err := d.layout()
	if err != nil {
		return nil
	}
	raw, ok := l.dirRecords(dir)
	if !ok {
		return nil
	}
	m := map[string][]fatEntry{}
	for _, e := range l.entries(raw) {
		if !e.deleted {
			k := diskfsName(e.name)
			m[k] = append(m[k], e)
		}
	}
	return m
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

func (d *diskfsDriver) layout() (*fatLayout, error) {
	if d.raw == nil {
		return nil, errors.ErrUnsupported
	}
	return readFATLayout(d.raw, d.kind)
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
