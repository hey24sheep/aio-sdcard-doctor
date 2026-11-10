package fsread

import (
	"fmt"
	"io"
	"strings"

	"github.com/aoiflux/libxfat"
)

// exfatDriver reads exFAT with aoiflux/libxfat (MIT).
type exfatDriver struct{ x *libxfat.ExFAT }

func openExFAT(r io.ReaderAt, size int64) (driver, error) {
	x, err := libxfat.Open(libxfat.Source{Reader: r, Size: size})
	if err != nil {
		return nil, err
	}
	return &exfatDriver{x: x}, nil
}

// maxDepth bounds path resolution (a looping directory structure can't recurse forever).
const maxDepth = 256

func live(e libxfat.Entry) bool {
	return !e.IsDeleted() && !e.IsVirtualEntry() && !e.IsRegion() && !e.IsBitmapUpcase() &&
		!e.IsSpecialFile() && !e.IsInvalid() && (e.IsDir() || e.IsFile())
}

// entries returns the live entries of dir, resolving it from the root one component at a time.
func (d *exfatDriver) entries(dir string) ([]libxfat.Entry, error) {
	es, err := d.x.ReadRootDir()
	if err != nil || dir == "." {
		return es, err
	}
	parts := strings.Split(dir, "/")
	if len(parts) > maxDepth {
		return nil, fmt.Errorf("path too deep")
	}
	for _, p := range parts {
		var next *libxfat.Entry
		for i := range es {
			if live(es[i]) && es[i].IsDir() && strings.EqualFold(es[i].Name(), p) {
				next = &es[i]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("%s: no such directory", dir)
		}
		if es, err = d.x.ReadDir(*next); err != nil {
			return nil, err
		}
	}
	return es, nil
}

func (d *exfatDriver) list(dir string) ([]node, error) {
	es, err := d.entries(dir)
	if err != nil {
		return nil, err
	}
	var out []node
	for _, e := range es {
		if live(e) {
			out = append(out, node{name: e.Name(), size: int64(e.Size()), mtime: e.ModifiedTime(), dir: e.IsDir()})
		}
	}
	return out, nil
}

func (d *exfatDriver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	dir, base := ".", name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		dir, base = name[:i], name[i+1:]
	}
	es, err := d.entries(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range es {
		if live(e) && !e.IsDir() && strings.EqualFold(e.Name(), base) {
			if e.Size() == 0 {
				return emptyReader{}, nopCloser{}, nil
			}
			f, err := d.x.OpenEntry(e)
			if err != nil {
				return nil, nil, err
			}
			ra, err := f.ReaderAt()
			if err != nil {
				return nil, nil, err
			}
			return ra, nopCloser{}, nil
		}
	}
	return nil, nil, fmt.Errorf("%s: not found", name)
}
