package fsread

import (
	"errors"
	"io"
	"strings"
	"sync"

	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

// ntfsDriver reads NTFS with Velocidex/go-ntfs (Apache-2.0).
//
// go-ntfs leaves out of a listing, without an error, a file whose record it can't read and an
// index block it can't decode. So folders are listed through a context of their own whose reads
// are watched: a read that fails during a listing makes it partial (ErrPartialFolder). Files are
// read through the other context, so their read errors never touch a listing.
type ntfsDriver struct {
	ctx   *ntfs.NTFSContext // files
	lctx  *ntfs.NTFSContext // listings (vol.list runs one at a time)
	lseen *failWatch
}

func openNTFS(r io.ReaderAt) (driver, error) {
	ctx, err := ntfsContext(r)
	if err != nil {
		return nil, err
	}
	w := &failWatch{r: r}
	lctx, err := ntfsContext(w)
	if err != nil {
		return nil, err
	}
	return &ntfsDriver{ctx: ctx, lctx: lctx, lseen: w}, nil
}

func ntfsContext(r io.ReaderAt) (*ntfs.NTFSContext, error) {
	paged, err := ntfs.NewPagedReader(r, 1024, 4096) // ≤ 4 MiB of cache
	if err != nil {
		return nil, err
	}
	return ntfs.GetNTFSContext(paged, 0)
}

// failWatch counts the reads that fail (the end of the volume aside).
type failWatch struct {
	r      io.ReaderAt
	mu     sync.Mutex
	failed int
}

func (w *failWatch) ReadAt(p []byte, off int64) (int, error) {
	n, err := w.r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		w.mu.Lock()
		w.failed++
		w.mu.Unlock()
	}
	return n, err
}

func (w *failWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (d *ntfsDriver) list(dir string) ([]node, error) {
	before := d.lseen.count()
	root, err := d.lctx.GetMFT(5)
	if err != nil {
		return nil, err
	}
	// one folder at a time, keeping each one's record number: a folder record pointing back at
	// one above it would repeat that part of the tree down to the depth cap
	e, up := root, map[uint32]bool{root.Record_number(): true}
	if dir != "." {
		for _, name := range strings.Split(dir, "/") {
			if e, err = e.Open(d.lctx, name); err != nil {
				return nil, err
			}
			if n := e.Record_number(); up[n] {
				return nil, errDirLoop
			} else {
				up[n] = true
			}
		}
	}
	seen := map[string]bool{}
	var out []node
	for _, fi := range ntfs.ListDir(d.lctx, e) {
		switch {
		case fi.IsSlack, fi.Name == "" || fi.Name == "." || fi.Name == "..", strings.Contains(fi.Name, ":"),
			dir == "." && strings.HasPrefix(fi.Name, "$"), seen[fi.Name]:
			continue // I30 slack, alternate streams, NTFS metadata files, duplicate names
		}
		seen[fi.Name] = true
		out = append(out, node{name: fi.Name, size: fi.Size, mtime: fi.Mtime, dir: fi.IsDir})
	}
	if d.lseen.count() != before {
		return out, ErrPartialFolder
	}
	return out, nil
}

func (d *ntfsDriver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	if n.size == 0 {
		return emptyReader{}, nopCloser{}, nil
	}
	r, err := ntfs.GetDataForPath(d.ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return r, nopCloser{}, nil
}
