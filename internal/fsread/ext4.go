package fsread

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"reflect"
	"sync"

	"github.com/masahiro331/go-ext4-filesystem/ext4"
)

// ext4Driver reads ext2/3/4 with masahiro331/go-ext4-filesystem (Apache-2.0).
type ext4Driver struct {
	fsys *ext4.FileSystem
	inos map[string]int64 // folder path → inode number (list runs one at a time, under vol.mu)
}

func openExt4(r io.ReaderAt, size int64) (driver, error) {
	f, err := ext4.NewFS(*io.NewSectionReader(r, 0, size), nil)
	if err != nil {
		return nil, err
	}
	return &ext4Driver{fsys: f, inos: map[string]int64{".": ext4RootIno}}, nil
}

const ext4RootIno = 2

func (d *ext4Driver) list(dir string) ([]node, error) {
	// a folder record pointing back at a folder above it would repeat that part of the tree
	// down to the depth cap: a folder's inode mustn't be one of its parents'
	if ino, ok := d.ino(dir); ok && dir != "." {
		for p := path.Dir(dir); ; p = path.Dir(p) {
			if up, ok := d.ino(p); ok && up == ino {
				return nil, errDirLoop
			}
			if p == "." {
				break
			}
		}
	}
	es, err := d.fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(d.inos) > maxCached/10 {
		clear(d.inos)
		d.inos["."] = ext4RootIno
	}
	var out []node
	for _, e := range es {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		if ino, ok := inodeOf(e); ok && e.IsDir() {
			d.inos[path.Join(dir, e.Name())] = ino
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			continue // symlinks, devices, sockets: not user files
		}
		out = append(out, node{name: e.Name(), size: fi.Size(), mtime: fi.ModTime(), dir: fi.IsDir()})
	}
	return out, nil
}

func (d *ext4Driver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	if n.size == 0 {
		return emptyReader{}, nopCloser{}, nil
	}
	if n.orig != "" { // go-ext4-filesystem opens by name, so only the first of the two
		return nil, nil, errors.New("another file in this folder has the same name (damaged folder); only the first can be read")
	}
	s := &seqReaderAt{open: func() (fs.File, error) { return d.fsys.Open(name) }}
	return s, s, nil
}

// ino is the inode number of the folder at p: kept when its parent was listed, else looked up.
func (d *ext4Driver) ino(p string) (int64, bool) {
	if n, ok := d.inos[p]; ok {
		return n, true
	}
	fi, err := d.fsys.Stat(p)
	if err != nil {
		return 0, false
	}
	return inodeOf(fi)
}

// inodeOf reads the inode number go-ext4-filesystem keeps in its FileInfo, which it doesn't
// export (Sys is nil). If a new version drops the field, folders pointing back up the tree are
// still stopped by the depth cap.
func inodeOf(x any) (int64, bool) {
	if e, ok := x.(fs.DirEntry); ok {
		fi, err := e.Info()
		if err != nil {
			return 0, false
		}
		x = fi
	}
	v := reflect.ValueOf(x)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("ino")
	if !f.IsValid() || f.Kind() != reflect.Int64 {
		return 0, false
	}
	return f.Int(), true
}

// seqReaderAt adapts a sequential-only file to io.ReaderAt. Rescue reads forward, which stays
// sequential; a backward read reopens the file (bounded by the file size).
type seqReaderAt struct {
	mu   sync.Mutex
	open func() (fs.File, error)
	f    fs.File
	pos  int64
}

func (s *seqReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil || off < s.pos {
		if s.f != nil {
			s.f.Close()
		}
		f, err := s.open()
		if err != nil {
			return 0, err
		}
		s.f, s.pos = f, 0
	}
	if skip := off - s.pos; skip > 0 {
		n, err := io.CopyN(io.Discard, s.f, skip)
		s.pos += n
		if err != nil {
			return 0, err
		}
	}
	n, err := io.ReadFull(s.f, p)
	s.pos += int64(n)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("ext4 read: %w", err)
	}
	return n, err
}

func (s *seqReaderAt) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}
