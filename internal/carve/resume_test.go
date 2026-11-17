package carve

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// staleMP4 is an MP4 header (ftyp, moov) whose mdat claims n bytes.
func staleMP4(n uint32) []byte {
	hdr := append(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", make([]byte, 200))...)
	md := []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), 'm', 'd', 'a', 't'}
	return append(hdr, md...)
}

// OR8/SR8: a find is not looked for again inside itself, but that was forgotten when a scan
// resumed from a checkpoint inside it: the resumed scan listed nested finds a whole scan doesn't.
func TestResumeInsideAFindKeepsItsNestedFindsOut(t *testing.T) {
	old := checkpointEvery
	checkpointEvery = 1 << 20
	defer func() { checkpointEvery = old }()
	buf := noise(10<<20, 3)
	copy(buf, staleMP4(8<<20))
	copy(buf[2<<20:], staleMP4(1<<20)) // inside the first one's claimed range
	full := scanAll(t, buf, Options{})
	for _, h := range full {
		if h.Offset == 2<<20 {
			t.Fatalf("setup: a whole scan finds the nested header: %+v", full)
		}
	}
	if len(full) == 0 || full[0].Offset != 0 {
		t.Fatalf("setup: hits %+v", full)
	}
	var got []Hit
	err := Scan(context.Background(), bytes.NewReader(buf), int64(len(buf)), Options{Found: full[:1]}, 1<<20,
		func(h Hit) error { got = append(got, h); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		if h.Offset == 2<<20 {
			t.Errorf("resumed scan found the nested header a whole scan leaves out: %+v", got)
		}
	}
}

// SR4: a ZIP entry whose data descriptor was wiped made the sizer try every later "PK" record
// as its end, each with an uncached 64 KiB read: 1.5 GB of reads for one 2 MB zip.
func TestWipedZipDescriptorCostsLittle(t *testing.T) {
	var zb bytes.Buffer
	w := zip.NewWriter(&zb)
	rnd := noise(300*5000, 9)
	for i := 0; i < 5000; i++ {
		f, err := w.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("f%05d.txt", i), Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		f.Write(rnd[i*300 : (i+1)*300])
	}
	w.Close()
	z := zb.Bytes()
	i := bytes.Index(z, []byte("PK\x07\x08"))
	copy(z[i:], make([]byte, 16)) // the first entry's descriptor
	img := make([]byte, 4<<20)
	copy(img[4096:], z)
	r := &countReader{r: bytes.NewReader(img)}
	within(t, 20*time.Second, func() {
		Scan(context.Background(), r, int64(len(img)), Options{}, 0, func(Hit) error { return nil }, nil)
	})
	if r.n > 64<<20 {
		t.Errorf("read %d MiB to scan a 4 MiB image", r.n>>20)
	}
}

// within fails the test if fn doesn't finish in d.
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v", d)
	}
}
