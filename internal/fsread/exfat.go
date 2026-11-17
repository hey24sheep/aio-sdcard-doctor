package fsread

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aoiflux/libxfat"
)

// exfatDriver reads exFAT with aoiflux/libxfat (MIT).
type exfatDriver struct{ x *libxfat.ExFAT }

func openExFAT(r io.ReaderAt, size int64) (driver, error) {
	x, err := libxfat.Open(libxfat.Source{Reader: r, Size: size})
	if err != nil {
		return nil, err
	}
	if _, err := x.ReadRootDir(); err != nil {
		// The root folder always follows its FAT chain, so one zeroed or unreadable FAT entry
		// loses the whole card. Its clusters almost always follow one another: read it that way.
		if p, ok := exfatRootChain(x, r); ok {
			if y, err := libxfat.Open(libxfat.Source{Reader: p, Size: size}); err == nil {
				if _, err := y.ReadRootDir(); err == nil {
					x = y
				}
			}
		}
	}
	return &exfatDriver{x: x}, nil
}

// maxRootClusters bounds how far exfatRootChain looks for the root folder's end.
const maxRootClusters = 256

// exfatRootChain gives a view of the volume whose FAT links the root folder's first cluster to
// the ones after it, up to the cluster holding its end-of-folder record. FAT sectors that can't be
// read show as zeros (only the root's chain is known), not as an error.
func exfatRootChain(x *libxfat.ExFAT, r io.ReaderAt) (io.ReaderAt, bool) {
	root, cs := x.RootDirCluster(), int64(x.ClusterSize())
	last := uint64(x.ClusterCount()) + 1
	buf := make([]byte, cs)
	n := uint32(0)
	for end := false; !end && n < maxRootClusters && uint64(root+n) <= last; n++ {
		if _, err := r.ReadAt(buf, x.ClusterHeapOffset()+int64(root+n-2)*cs); err != nil {
			break
		}
		for i := 0; i < len(buf) && !end; i += 32 {
			end = buf[i] == 0 // the end-of-folder record
		}
	}
	if n == 0 {
		return nil, false
	}
	links := make([]byte, 4*n)
	for i := uint32(0); i < n; i++ {
		next := root + i + 1
		if i == n-1 {
			next = 0xFFFFFFFF
		}
		binary.LittleEndian.PutUint32(links[4*i:], next)
	}
	fat := x.ActiveFatOffset()
	return fatPatch{r: r, at: fat + int64(root)*4, b: links, fat: fat, fatEnd: fat + int64(x.FatSize()),
		sector: int64(x.BytesPerSector())}, true
}

// fatPatch reads r with b laid over it at offset at.
type fatPatch struct {
	r                   io.ReaderAt
	at                  int64
	b                   []byte
	fat, fatEnd, sector int64
}

func (p fatPatch) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.r.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) && off >= p.fat && off+int64(len(b)) <= p.fatEnd {
		n, err = len(b), nil
		for o := int64(0); o < int64(len(b)); o += p.sector {
			s := b[o:min(o+p.sector, int64(len(b)))]
			if _, e := p.r.ReadAt(s, off+o); e != nil && !errors.Is(e, io.EOF) {
				clear(s)
			}
		}
	}
	if lo, hi := max(off, p.at), min(off+int64(len(b)), p.at+int64(len(p.b))); lo < hi {
		copy(b[lo-off:hi-off], p.b[lo-p.at:hi-p.at])
	}
	return n, err
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
	// the first cluster of each folder on the way: a folder record pointing back at one of them
	// would repeat that part of the tree down to the depth cap
	up := map[uint32]bool{d.x.RootDirCluster(): true}
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
		if c := next.FirstCluster(); c != 0 {
			if up[c] {
				return nil, errDirLoop
			}
			up[c] = true
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
			out = append(out, node{name: e.Name(), size: int64(e.Size()), mtime: exfatTime(e), dir: e.IsDir()})
		}
	}
	return out, nil
}

// exfatTime is an entry's modified time. One stored without a UTC offset is the writer's wall
// clock, which the OS shows as local time (as for FAT); libxfat would label it UTC.
func exfatTime(e libxfat.Entry) time.Time {
	ts := e.Timestamps()
	if ts.ModifiedOffsetValid || ts.ModifiedLocal.IsZero() {
		return ts.Modified
	}
	return localTime(ts.ModifiedLocal)
}

func (d *exfatDriver) open(name string, n node) (io.ReaderAt, io.Closer, error) {
	dir, base := ".", name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		dir, base = name[:i], name[i+1:]
	}
	if n.orig != "" { // a name the folder holds twice: open that record, not the first
		base = n.orig
	}
	es, err := d.entries(dir)
	if err != nil {
		return nil, nil, err
	}
	var found *libxfat.Entry // the nth record spelled exactly so, else one ignoring case
	for i := range es {
		if e := es[i]; live(e) && !e.IsDir() && e.Name() == base {
			if n.nth == 0 {
				found = &es[i]
				break
			}
			n.nth--
		}
	}
	for i := 0; i < len(es) && found == nil; i++ {
		if e := es[i]; live(e) && !e.IsDir() && strings.EqualFold(e.Name(), base) {
			found = &es[i]
		}
	}
	if found == nil {
		return nil, nil, fmt.Errorf("%s: not found", name)
	}
	if found.Size() == 0 {
		return emptyReader{}, nopCloser{}, nil
	}
	f, err := d.x.OpenEntry(*found)
	if err != nil {
		return nil, nil, err
	}
	ra, err := f.ReaderAt()
	if err != nil {
		return nil, nil, err
	}
	return ra, nopCloser{}, nil
}
