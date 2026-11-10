package imaging

import (
	"bytes"
	"strings"
	"testing"
)

const ddrescueSample = `# Mapfile. Created by GNU ddrescue version 1.27
# Command line: ddrescue /dev/sdb card.img card.map
# Start time:   2026-11-09 10:00:00
# Current time: 2026-11-09 10:05:00
# Copying non-tried blocks... Pass 1 (forwards)
# current_pos  current_status  current_pass
0x00200000     ?               1
#      pos        size  status
0x00000000  0x00100000  +
0x00100000  0x00010000  *
0x00110000  0x000F0000  ?
0x00200000  0x00E00000  ?
`

func TestParseDdrescueSample(t *testing.T) {
	m, err := ParseMapfile(strings.NewReader(ddrescueSample))
	if err != nil {
		t.Fatal(err)
	}
	if m.CurPos != 0x200000 || m.CurStatus != '?' || m.Pass != 1 || len(m.Blocks) != 3 {
		t.Fatalf("%+v", m) // adjacent '?' blocks merge
	}
	if m.Blocks[1] != (Block{Pos: 0x100000, Size: 0x10000, Status: '*'}) {
		t.Fatalf("%+v", m.Blocks[1])
	}
	if m.Size() != 0x1000000 || m.Bytes('+') != 0x100000 {
		t.Fatalf("size %d finished %d", m.Size(), m.Bytes('+'))
	}
}

func TestMapfileRoundTrip(t *testing.T) {
	m := NewMapfile(10 << 20)
	m.Set(0, 1<<20, '+')
	m.Set(3<<20, 4096, '-')
	m.Set(3<<20+4096, 8192, '/')
	m.Pass, m.CurPos, m.CurStatus = 3, 3<<20, '*'
	var b bytes.Buffer
	if err := m.Write(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "# current_pos  current_status  current_pass") {
		t.Fatalf("not ddrescue format:\n%s", b.String())
	}
	m2, err := ParseMapfile(&b)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Pass != 3 || m2.CurPos != 3<<20 || m2.CurStatus != '*' || len(m2.Blocks) != len(m.Blocks) {
		t.Fatalf("%+v vs %+v", m2, m)
	}
	for i := range m.Blocks {
		if m.Blocks[i] != m2.Blocks[i] {
			t.Fatalf("block %d: %+v vs %+v", i, m.Blocks[i], m2.Blocks[i])
		}
	}
}

func TestMapfileSetSplitsAndMerges(t *testing.T) {
	m := NewMapfile(100)
	m.Set(10, 10, '+')
	m.Set(20, 10, '+')
	m.Set(15, 2, '-')
	want := []Block{{0, 10, '?'}, {10, 5, '+'}, {15, 2, '-'}, {17, 13, '+'}, {30, 70, '?'}}
	if len(m.Blocks) != len(want) {
		t.Fatalf("%+v", m.Blocks)
	}
	for i := range want {
		if m.Blocks[i] != want[i] {
			t.Fatalf("%+v", m.Blocks)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "0x10 ? 1\n0x0 0x10 Z\n", "0x0 ? 1\n0x10 0x10 +\n"} {
		if _, err := ParseMapfile(strings.NewReader(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
