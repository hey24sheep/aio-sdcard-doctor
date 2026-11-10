// Package imaging makes a full backup image of a card, ddrescue-style: good areas first, then
// the hard ones, with a GNU ddrescue-compatible mapfile so the run resumes exactly and the map
// opens in ddrescue/ddrescueview.
package imaging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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

// maxMapLines bounds parsing (a corrupt or hostile map can't exhaust memory).
const maxMapLines = 1 << 20

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

// Set marks [pos, pos+size) with st, splitting and merging blocks.
func (m *Mapfile) Set(pos, size int64, st byte) {
	if size <= 0 {
		return
	}
	end := pos + size
	out := make([]Block, 0, len(m.Blocks)+2)
	inserted := false
	for _, b := range m.Blocks {
		be := b.Pos + b.Size
		if be <= pos || b.Pos >= end {
			if b.Pos >= end && !inserted {
				out = append(out, Block{pos, size, st})
				inserted = true
			}
			out = append(out, b)
			continue
		}
		if b.Pos < pos {
			out = append(out, Block{b.Pos, pos - b.Pos, b.Status})
		}
		if !inserted {
			out = append(out, Block{pos, size, st})
			inserted = true
		}
		if be > end {
			out = append(out, Block{end, be - end, b.Status})
		}
	}
	if !inserted {
		out = append(out, Block{pos, size, st})
	}
	m.Blocks = merge(out)
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
func ParseMapfile(r io.Reader) (*Mapfile, error) {
	sc := bufio.NewScanner(r)
	m := &Mapfile{}
	haveStatus := false
	for lines := 0; sc.Scan(); lines++ {
		if lines > maxMapLines {
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
