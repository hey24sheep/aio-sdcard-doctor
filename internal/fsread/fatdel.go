package fsread

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// FAT deleted entries and live cluster runs. go-diskfs only lists live entries, so this reads the
// FAT and the directory records itself — read-only, from the same ReaderAt.

type fatLayout struct {
	r           io.ReaderAt
	cluster     int64 // bytes per cluster
	dataOff     int64
	rootOff     int64 // FAT12/16 fixed root directory
	rootLen     int64
	rootClus    uint32 // FAT32
	nclus       uint32 // data clusters (numbered 2..nclus+1)
	fat         []uint32
	bits        int
	maxEntries  int
	visitedDirs map[uint32]bool
}

const fatEOC = 0x0FFFFFF8

var errFATLayout = errors.New("fat: unreadable boot sector or FAT")

func readFATLayout(r io.ReaderAt, kind string) (*fatLayout, error) {
	bs := make([]byte, 512)
	if _, err := r.ReadAt(bs, 0); err != nil {
		return nil, err
	}
	bps, spc := int64(le16(bs[11:])), int64(bs[13])
	reserved, nfats, rootEnts := int64(le16(bs[14:])), int64(bs[16]), int64(le16(bs[17:]))
	total := int64(le16(bs[19:]))
	if total == 0 {
		total = int64(le32(bs[32:]))
	}
	fatSecs := int64(le16(bs[22:]))
	if fatSecs == 0 {
		fatSecs = int64(le32(bs[36:]))
	}
	if bps < 512 || bps&(bps-1) != 0 || spc == 0 || spc&(spc-1) != 0 || nfats == 0 || fatSecs == 0 {
		return nil, errFATLayout
	}
	l := &fatLayout{r: r, cluster: bps * spc, maxEntries: 2_000_000, visitedDirs: map[uint32]bool{}}
	fatOff := reserved * bps
	l.rootOff = fatOff + nfats*fatSecs*bps
	l.rootLen = (rootEnts*32 + bps - 1) / bps * bps
	l.dataOff = l.rootOff + l.rootLen
	if total*bps <= l.dataOff {
		return nil, errFATLayout
	}
	l.nclus = uint32((total*bps - l.dataOff) / l.cluster)
	switch kind {
	case "fat12":
		l.bits = 12
	case "fat16":
		l.bits = 16
	default:
		l.bits = 32
		l.rootClus = le32(bs[44:])
	}
	raw := make([]byte, fatSecs*bps)
	if _, err := r.ReadAt(raw, fatOff); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// some formatters size the FAT a little short of the data area: clusters it can't number
	// can't hold files either
	if fatCap := uint32(int64(len(raw)) * 8 / int64(l.bits)); fatCap < l.nclus+2 {
		if fatCap < 3 {
			return nil, errFATLayout
		}
		l.nclus = fatCap - 2
	}
	n := l.nclus + 2
	l.fat = make([]uint32, n)
	for c := uint32(0); c < n; c++ {
		var v uint32
		switch l.bits {
		case 12:
			o := int(c) * 3 / 2
			if o+1 >= len(raw) {
				return nil, errFATLayout
			}
			v = uint32(le16(raw[o:]))
			if c&1 == 1 {
				v >>= 4
			}
			v &= 0xFFF
			if v >= 0xFF8 {
				v = fatEOC
			}
		case 16:
			if int(c)*2+1 >= len(raw) {
				return nil, errFATLayout
			}
			v = uint32(le16(raw[c*2:]))
			if v >= 0xFFF8 {
				v = fatEOC
			}
		default:
			if int(c)*4+3 >= len(raw) {
				return nil, errFATLayout
			}
			v = le32(raw[c*4:]) & 0x0FFFFFFF
		}
		l.fat[c] = v
	}
	return l, nil
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 { return uint32(le16(b)) | uint32(le16(b[2:]))<<16 }

func (l *fatLayout) valid(c uint32) bool  { return c >= 2 && c < l.nclus+2 }
func (l *fatLayout) free(c uint32) bool   { return l.valid(c) && l.fat[c] == 0 }
func (l *fatLayout) off(c uint32) int64   { return l.dataOff + int64(c-2)*l.cluster }
func (l *fatLayout) runOf(size int64) int { return int((size + l.cluster - 1) / l.cluster) }

// chain follows a live cluster chain (loops and bad links end it).
func (l *fatLayout) chain(c uint32) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	for l.valid(c) && !seen[c] && len(out) <= int(l.nclus) {
		seen[c] = true
		out = append(out, c)
		c = l.fat[c]
	}
	return out
}

type fatEntry struct {
	name    string
	deleted bool
	dir     bool
	clus    uint32
	size    int64
	mtime   time.Time
}

func lfnChars(b []byte) []uint16 {
	var u []uint16
	for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
		for i := r[0]; i < r[1]; i += 2 {
			u = append(u, uint16(le16(b[i:])))
		}
	}
	return u
}

func shortSum(n []byte) byte {
	var s byte
	for _, c := range n[:11] {
		s = (s&1)<<7 + s>>1 + c
	}
	return s
}

func fatTime(d, t uint16) time.Time {
	if d == 0 {
		return time.Time{}
	}
	return time.Date(1980+int(d>>9), time.Month(d>>5&15), int(d&31), int(t>>11), int(t>>5&63), int(t&31)*2, 0, time.Local)
}

// entries parses one directory's records (all of them, deleted included).
func (l *fatLayout) entries(raw []byte) []fatEntry {
	var out []fatEntry
	var lfn [][]uint16
	var lfnSum byte
	for i := 0; i+32 <= len(raw) && len(out) < l.maxEntries; i += 32 {
		e := raw[i : i+32]
		if e[0] == 0 {
			break
		}
		attr := e[11]
		if attr == 0x0F {
			if e[0]&0x40 != 0 && e[0] != 0xE5 || len(lfn) == 0 {
				lfn = nil
			}
			lfn = append(lfn, lfnChars(e))
			lfnSum = e[13]
			continue
		}
		parts := lfn
		lfn = nil
		if attr&0x08 != 0 || e[0] == '.' {
			continue
		}
		del := e[0] == 0xE5
		name := ""
		if len(parts) > 0 {
			ok := shortSum(e) == lfnSum
			if del { // the first byte was overwritten: try every original first character
				n := append([]byte{}, e[:11]...)
				for c := 0x20; c < 0x100 && !ok; c++ {
					n[0] = byte(c)
					ok = shortSum(n) == lfnSum
				}
			}
			if ok {
				var u []uint16
				for j := len(parts) - 1; j >= 0; j-- {
					u = append(u, parts[j]...)
				}
				for k, c := range u {
					if c == 0 {
						u = u[:k]
						break
					}
				}
				name = string(utf16.Decode(u))
			}
		}
		if name == "" {
			b := append([]byte{}, e[:8]...)
			if del {
				b[0] = '_'
			} else if b[0] == 0x05 {
				b[0] = 0xE5
			}
			base := strings.TrimRight(string(b), " ")
			ext := strings.TrimRight(string(e[8:11]), " ")
			if e[12]&0x08 != 0 {
				base = strings.ToLower(base)
			}
			if e[12]&0x10 != 0 {
				ext = strings.ToLower(ext)
			}
			name = base
			if ext != "" {
				name += "." + ext
			}
		}
		clus := uint32(le16(e[26:]))
		if l.bits == 32 {
			clus |= uint32(le16(e[20:])) << 16
		}
		out = append(out, fatEntry{name: name, deleted: del, dir: attr&0x10 != 0, clus: clus,
			size: int64(le32(e[28:])), mtime: fatTime(le16(e[24:]), le16(e[22:]))})
	}
	return out
}

func (l *fatLayout) read(off, n int64) []byte {
	b := make([]byte, n)
	got, _ := l.r.ReadAt(b, off)
	return b[:got]
}

// readDir reads a live directory (its chain) or, for a deleted one, its first cluster only.
func (l *fatLayout) readDir(clus uint32, deleted bool) []byte {
	if clus == 0 && l.bits != 32 {
		return l.read(l.rootOff, l.rootLen)
	}
	if clus == 0 {
		clus = l.rootClus
	}
	if !l.valid(clus) || l.visitedDirs[clus] {
		return nil
	}
	l.visitedDirs[clus] = true
	if deleted {
		return l.read(l.off(clus), l.cluster)
	}
	var b []byte
	for _, c := range l.chain(clus) {
		b = append(b, l.read(l.off(c), l.cluster)...)
	}
	return b
}

// walk visits every entry depth first; inGone is true below a deleted directory.
func (l *fatLayout) walk(ctx context.Context, clus uint32, dir string, gone bool, depth int,
	fn func(path string, e fatEntry, gone bool) error) error {
	if depth > maxDepth {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw := l.readDir(clus, gone)
	if gone && (len(raw) < 64 || raw[0] != '.' || raw[32] != '.') {
		return nil // a deleted directory whose first cluster no longer holds its records
	}
	for _, e := range l.entries(raw) {
		p := e.name
		if dir != "" {
			p = dir + "/" + e.name
		}
		g := gone || e.deleted
		if err := fn(p, e, g); err != nil {
			return err
		}
		if e.dir && l.valid(e.clus) && (!g || l.free(e.clus)) { // cluster 0 would re-read the root
			if err := l.walk(ctx, e.clus, p, g, depth+1, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// classify a deleted file assumed contiguous from its first cluster (its chain was freed).
func (l *fatLayout) classify(e fatEntry) (string, [][2]int64) {
	if e.size == 0 || !l.valid(e.clus) {
		return "metadata-damaged", nil
	}
	n := l.runOf(e.size)
	if uint32(n) > l.nclus || !l.valid(e.clus+uint32(n)-1) {
		return "metadata-damaged", nil
	}
	ext := [][2]int64{{l.off(e.clus), e.size}}
	used := 0
	for c := e.clus; c < e.clus+uint32(n); c++ {
		if !l.free(c) {
			used++
		}
	}
	switch {
	case used == 0:
		return "recoverable", ext
	case used == n:
		return "overwritten", ext
	}
	return "fragmented-unknown", ext
}

func (l *fatLayout) deleted(ctx context.Context, emit func(source.Deleted) error) error {
	return l.walk(ctx, 0, "", false, 0, func(p string, e fatEntry, gone bool) error {
		if !gone || e.dir {
			return nil
		}
		st, ext := l.classify(e)
		return emit(source.Deleted{Path: p, Size: e.size, Extents: ext, State: st, MTime: e.mtime})
	})
}

func (l *fatLayout) live(ctx context.Context, emit func(path string, off, n int64) error) error {
	return l.walk(ctx, 0, "", false, 0, func(p string, e fatEntry, gone bool) error {
		if gone || e.dir || e.size == 0 {
			return nil
		}
		cs := l.chain(e.clus)
		left := e.size
		for i := 0; i < len(cs) && left > 0; {
			j := i + 1
			for j < len(cs) && cs[j] == cs[j-1]+1 {
				j++
			}
			n := min(int64(j-i)*l.cluster, left)
			if err := emit(p, l.off(cs[i]), n); err != nil {
				return err
			}
			left -= n
			i = j
		}
		return nil
	})
}
