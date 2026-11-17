// Package imaging makes a full backup image of a card, ddrescue-style: good areas first, then
// the hard ones, with a GNU ddrescue-compatible mapfile so the run resumes exactly and the map
// opens in ddrescue/ddrescueview.
package imaging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Block statuses (GNU ddrescue).
const (
	NonTried   = '?'
	NonTrimmed = '*'
	NonScraped = '/'
	BadSector  = '-'
	Finished   = '+'
)

// Block is a run of the device with one status.
type Block struct {
	Pos, Size int64
	Status    byte
}

// Mapfile is the ddrescue map: where the run is, and the status of every byte.
type Mapfile struct {
	CurPos    int64
	CurStatus byte
	Pass      int
	Blocks    []Block
}

// maxMapLines bounds parsing (a corrupt or hostile map can't exhaust memory) when the card's size
// isn't known; mapLines gives the bound for a card of a known size.
const maxMapLines = 1 << 20

// mapLines is the most lines a map of a device of size bytes can need: a block per 512-byte
// sector, and the comment and status lines.
func mapLines(size int64) int {
	return int(max(maxMapLines, min(size/512+64, 1<<40)))
}

// NewMapfile is an untried map of a device of size bytes.
func NewMapfile(size int64) *Mapfile {
	return &Mapfile{CurStatus: NonTried, Pass: 1, Blocks: []Block{{0, size, NonTried}}}
}

// Size is the device size the map covers.
func (m *Mapfile) Size() int64 {
	if len(m.Blocks) == 0 {
		return 0
	}
	b := m.Blocks[len(m.Blocks)-1]
	return b.Pos + b.Size
}

// Bytes counts bytes with status st.
func (m *Mapfile) Bytes(st byte) int64 {
	var n int64
	for _, b := range m.Blocks {
		if b.Status == st {
			n += b.Size
		}
	}
	return n
}

// statusAt is the status of the byte at pos (0 outside the map).
func (m *Mapfile) statusAt(pos int64) byte {
	i := sort.Search(len(m.Blocks), func(i int) bool { return m.Blocks[i].Pos+m.Blocks[i].Size > pos })
	if i < len(m.Blocks) && m.Blocks[i].Pos <= pos {
		return m.Blocks[i].Status
	}
	return 0
}

// Set marks [pos, pos+size) with st, splitting and merging blocks. Only the blocks it touches
// are found (binary search) and replaced in place: a damaged card's map holds many blocks.
func (m *Mapfile) Set(pos, size int64, st byte) {
	if size <= 0 {
		return
	}
	end, bs := pos+size, m.Blocks
	i := sort.Search(len(bs), func(k int) bool { return bs[k].Pos+bs[k].Size > pos }) // first touched
	j := sort.Search(len(bs), func(k int) bool { return bs[k].Pos >= end })           // first after
	j = max(i, j)
	var mid [5]Block // what replaces bs[lo:hi]: neighbours it merges with, the parts left, the new one
	r := mid[:0]
	lo, hi := i, j
	if lo > 0 {
		lo--
		r = append(r, bs[lo])
	}
	if i < j && bs[i].Pos < pos {
		r = append(r, Block{bs[i].Pos, pos - bs[i].Pos, bs[i].Status})
	}
	r = append(r, Block{pos, size, st})
	if i < j {
		if last := bs[j-1]; last.Pos+last.Size > end {
			r = append(r, Block{end, last.Pos + last.Size - end, last.Status})
		}
	}
	if hi < len(bs) {
		r = append(r, bs[hi])
		hi++
	}
	m.Blocks = slices.Replace(bs, lo, hi, merge(r)...)
}

func merge(bs []Block) []Block {
	out := bs[:0]
	for _, b := range bs {
		if n := len(out); n > 0 && out[n-1].Status == b.Status && out[n-1].Pos+out[n-1].Size == b.Pos {
			out[n-1].Size += b.Size
			continue
		}
		out = append(out, b)
	}
	return out
}

// next returns the first block with status st that ends after from.
func (m *Mapfile) next(st byte, from int64) (Block, bool) {
	for _, b := range m.Blocks {
		if b.Status == st && b.Pos+b.Size > from {
			if b.Pos < from {
				b.Size -= from - b.Pos
				b.Pos = from
			}
			return b, true
		}
	}
	return Block{}, false
}

func validStatus(c byte) bool { return strings.IndexByte("?*/-+", c) >= 0 }

// ParseMapfile reads a GNU ddrescue mapfile.
func ParseMapfile(r io.Reader) (*Mapfile, error) { return parseMapfile(r, maxMapLines) }

func parseMapfile(r io.Reader, maxLines int) (*Mapfile, error) {
	sc := bufio.NewScanner(r)
	m := &Mapfile{}
	haveStatus := false
	for lines := 0; sc.Scan(); lines++ {
		if lines > maxLines {
			return nil, errors.New("mapfile: too many lines")
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if !haveStatus {
			if len(f) < 2 {
				return nil, fmt.Errorf("mapfile: bad status line %q", line)
			}
			pos, err := strconv.ParseInt(f[0], 0, 64)
			if err != nil || len(f[1]) != 1 {
				return nil, fmt.Errorf("mapfile: bad status line %q", line)
			}
			m.CurPos, m.CurStatus, m.Pass = pos, f[1][0], 1
			if len(f) > 2 {
				if m.Pass, err = strconv.Atoi(f[2]); err != nil {
					return nil, fmt.Errorf("mapfile: bad pass %q", f[2])
				}
			}
			haveStatus = true
			continue
		}
		if len(f) != 3 || len(f[2]) != 1 || !validStatus(f[2][0]) {
			return nil, fmt.Errorf("mapfile: bad block line %q", line)
		}
		pos, err1 := strconv.ParseInt(f[0], 0, 64)
		size, err2 := strconv.ParseInt(f[1], 0, 64)
		if err1 != nil || err2 != nil || size <= 0 || pos != m.Size() {
			return nil, fmt.Errorf("mapfile: bad or non-contiguous block %q", line)
		}
		m.Blocks = append(m.Blocks, Block{pos, size, f[2][0]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !haveStatus || len(m.Blocks) == 0 {
		return nil, errors.New("mapfile: empty")
	}
	m.Blocks = merge(m.Blocks)
	return m, nil
}

// Write writes the map in GNU ddrescue format.
func (m *Mapfile) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# Mapfile. Created by aio-sdcard-doctor (GNU ddrescue compatible)\n")
	fmt.Fprintf(bw, "# Current time: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(bw, "# current_pos  current_status  current_pass\n")
	fmt.Fprintf(bw, "0x%08X     %c               %d\n", m.CurPos, m.CurStatus, m.Pass)
	fmt.Fprintf(bw, "#      pos        size  status\n")
	for _, b := range m.Blocks {
		fmt.Fprintf(bw, "0x%08X  0x%08X  %c\n", b.Pos, b.Size, b.Status)
	}
	return bw.Flush()
}
