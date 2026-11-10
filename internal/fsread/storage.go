// Package fsread reads a card's file system directly from a raw device or image, through
// permissively licensed open-source parsers that only ever get a read-only io.ReaderAt. It runs
// in the worker process, so a parser that hangs or crashes on a damaged card cannot take the
// app down.
package fsread

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/backend"
)

// storage is a read-only backend.Storage for go-diskfs over our io.ReaderAt. Writable and Sys
// always fail, so go-diskfs can never write through it.
type storage struct {
	r         io.ReaderAt
	size, pos int64
}

func newStorage(r io.ReaderAt, size int64) *storage { return &storage{r: r, size: size} }

var errReadOnly = errors.New("fsread: storage is read-only")

func (s *storage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := p
	if rest := s.size - off; int64(len(p)) > rest {
		want = p[:rest]
	}
	n, err := s.r.ReadAt(want, off)
	if err == nil && len(want) < len(p) {
		err = io.EOF
	}
	return n, err
}

func (s *storage) Read(p []byte) (int, error) {
	n, err := s.ReadAt(p, s.pos)
	s.pos += int64(n)
	return n, err
}

func (s *storage) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += s.pos
	case io.SeekEnd:
		off += s.size
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	s.pos = off
	return off, nil
}

func (s *storage) Close() error                            { return nil }
func (s *storage) Sys() (*os.File, error)                  { return nil, errReadOnly }
func (s *storage) Writable() (backend.WritableFile, error) { return nil, backend.ErrIncorrectOpenMode }
func (s *storage) Path() string                            { return "" }
func (s *storage) Stat() (fs.FileInfo, error)              { return storageInfo{s.size}, nil }

type storageInfo struct{ size int64 }

func (i storageInfo) Name() string       { return "card" }
func (i storageInfo) Size() int64        { return i.size }
func (i storageInfo) Mode() fs.FileMode  { return 0o444 }
func (i storageInfo) ModTime() time.Time { return time.Time{} }
func (i storageInfo) IsDir() bool        { return false }
func (i storageInfo) Sys() any           { return nil }
