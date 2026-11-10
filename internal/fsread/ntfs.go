package fsread

import (
	"io"
	"strings"

	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

// ntfsDriver reads NTFS with Velocidex/go-ntfs (Apache-2.0).
type ntfsDriver struct{ ctx *ntfs.NTFSContext }

func openNTFS(r io.ReaderAt) (driver, error) {
	paged, err := ntfs.NewPagedReader(r, 1024, 4096) // ≤ 4 MiB of cache
	if err != nil {
		return nil, err
	}
	ctx, err := ntfs.GetNTFSContext(paged, 0)
	if err != nil {
		return nil, err
	}
	return &ntfsDriver{ctx: ctx}, nil
}

func (d *ntfsDriver) list(dir string) ([]node, error) {
	root, err := d.ctx.GetMFT(5)
	if err != nil {
		return nil, err
	}
	e := root
	if dir != "." {
		if e, err = root.Open(d.ctx, dir); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var out []node
	for _, fi := range ntfs.ListDir(d.ctx, e) {
		switch {
		case fi.IsSlack, fi.Name == "" || fi.Name == "." || fi.Name == "..", strings.Contains(fi.Name, ":"),
			dir == "." && strings.HasPrefix(fi.Name, "$"), seen[fi.Name]:
			continue // I30 slack, alternate streams, NTFS metadata files, duplicate names
		}
		seen[fi.Name] = true
		out = append(out, node{name: fi.Name, size: fi.Size, mtime: fi.Mtime, dir: fi.IsDir})
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
