package fsread

import (
	"context"
	"errors"
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
	var real map[string]string // looked up only when a name came back mangled
	var out []node
	for _, e := range es {
		if e.Name() == "." || e.Name() == ".." || e.Name() == "" {
			continue
		}
		name := e.Name()
		if strings.ContainsRune(name, utf8.RuneError) && d.raw != nil {
			if real == nil {
				real = d.realNames(dir)
			}
			if r, ok := real[name]; ok {
				name = r
			}
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		mt := fi.ModTime()
		if d.localTime {
			mt = localTime(mt)
		}
		out = append(out, node{name: name, size: fi.Size(), mtime: mt, dir: fi.IsDir()})
	}
	return out, nil
}

func (d *diskfsDriver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	if n.size == 0 {
		return emptyReader{}, nopCloser{}, nil
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

// realNames maps go-diskfs's spelling of each live name in dir to the real name.
func (d *diskfsDriver) realNames(dir string) map[string]string {
	l, err := d.layout()
	if err != nil {
		return nil
	}
	raw := l.readDir(0, false)
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || seg == "." {
			continue
		}
		found := false
		for _, e := range l.entries(raw) {
			if !e.deleted && e.dir && e.name == seg && l.valid(e.clus) {
				raw, found = l.readDir(e.clus, false), true
				break
			}
		}
		if !found {
			return nil
		}
	}
	m := map[string]string{}
	for _, e := range l.entries(raw) {
		if !e.deleted {
			m[diskfsName(e.name)] = e.name
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
