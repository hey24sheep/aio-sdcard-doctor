package fsread

import (
	"fmt"
	"io"
	"io/fs"
	"sync"

	"github.com/masahiro331/go-ext4-filesystem/ext4"
)

// ext4Driver reads ext2/3/4 with masahiro331/go-ext4-filesystem (Apache-2.0).
type ext4Driver struct{ fsys *ext4.FileSystem }

func openExt4(r io.ReaderAt, size int64) (driver, error) {
	f, err := ext4.NewFS(*io.NewSectionReader(r, 0, size), nil)
	if err != nil {
		return nil, err
	}
	return &ext4Driver{fsys: f}, nil
}

func (d *ext4Driver) list(dir string) ([]node, error) {
	es, err := d.fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []node
	for _, e := range es {
		if e.Name() == "." || e.Name() == ".." {
			continue
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
	s := &seqReaderAt{open: func() (fs.File, error) { return d.fsys.Open(name) }}
	return s, s, nil
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
