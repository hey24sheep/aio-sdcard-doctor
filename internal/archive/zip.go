package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zip"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/salvage"
)

// MaxPartBytes closes a ZIP part, and a file that would push a part past it starts a new one, so
// a part stays well under FAT32's 4 GiB unless one file is that big (a variable for tests).
var MaxPartBytes int64 = 1 << 30

const (
	// MaxEntries closes a ZIP part (bounding what a crash can cost); see also MaxPartBytes.
	MaxEntries = 1000
	// storeBelow: entries whose first MiB shrinks by less than 1% are stored, not deflated.
	storeBelow  = 0.99
	sampleBytes = 1 << 20
	indexBucket = "zipindex"
	spoolName   = ".spool.tmp"
)

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type pending struct {
	stored, name string
	size         int64
	sum          [32]byte
}

type part struct {
	n       int
	tmp     string
	f       destfs.File
	cw      *countWriter
	zw      *zip.Writer
	entries []pending
}

type zipSink struct {
	out    destfs.FS
	root   string
	st     *job.Store
	open   map[string]*part
	failed []string
}

// ZipSink stores files in <root>/<Category>-NNNN.zip parts (Deflate level 9, ZIP64 when needed).
// Each file is first written to a spool file, fsync'd and verified, so a worse copy is never
// added; a part is written as .zip.tmp and only renamed — and its entries only reported as
// stored — once it is closed and re-read successfully. A crash loses at most the open part.
func ZipSink(out destfs.FS, root string, st *job.Store) Sink {
	s := &zipSink{out: out, root: root, st: st, open: map[string]*part{}}
	if es, err := os.ReadDir(root); err == nil {
		for _, e := range es {
			if strings.HasSuffix(e.Name(), ".zip.tmp") || e.Name() == spoolName {
				out.Remove(filepath.Join(root, e.Name())) // ours: an unfinished part from a crash
			}
		}
	}
	return s
}

// Failed lists stored names whose part failed verification (to be reported FAILED).
func (s *zipSink) Failed() []string { return s.failed }

// ZipIndexHas reports whether dest ("Photos-0001.zip/DCIM/a.jpg") is in a verified, closed
// ZIP part recorded in st.
func ZipIndexHas(st *job.Store, dest string, size int64) bool {
	v, ok, err := job.Get[int64](st, indexBucket, dest)
	return err == nil && ok && v == size
}

// ZipPart splits a ZIP destination ("Photos-0001.zip/DCIM/a.jpg") into its part file and the
// name inside it; ok is false for a folder destination, even a card folder named "pics.zip".
func ZipPart(dest string) (part, name string, ok bool) {
	part, name, ok = strings.Cut(dest, "/")
	return part, name, ok && partRe.MatchString(part)
}

// ZipParts remembers which names each ZIP part on the destination really holds (read once).
type ZipParts map[string]map[string]uint64

// Stored reports whether dest is recorded in st's index with this size and its part file in
// root still holds it: a part deleted or replaced after the run doesn't count as stored.
func (zp ZipParts) Stored(st *job.Store, root, dest string, size int64) bool {
	part, name, ok := ZipPart(dest)
	if !ok || !ZipIndexHas(st, dest, size) {
		return false
	}
	names, seen := zp[part]
	if !seen {
		names = map[string]uint64{}
		if zr, err := zip.OpenReader(filepath.Join(root, part)); err == nil {
			for _, f := range zr.File {
				names[f.Name] = f.UncompressedSize64
			}
			zr.Close()
		}
		zp[part] = names
	}
	n, ok := names[name]
	return ok && n == uint64(size)
}

func (s *zipSink) Has(dest string, size int64) bool {
	v, ok, err := job.Get[int64](s.st, indexBucket, dest)
	return err == nil && ok && v == size
}

var partRe = regexp.MustCompile(`^(.+)-(\d{4,})\.zip$`)

func (s *zipSink) nextNumber(cat string) int {
	n := 0
	if es, err := os.ReadDir(s.root); err == nil {
		for _, e := range es {
			if m := partRe.FindStringSubmatch(e.Name()); m != nil && m[1] == cat {
				if k, _ := strconv.Atoi(m[2]); k > n {
					n = k
				}
			}
		}
	}
	return n + 1
}

func (s *zipSink) partFor(cat string) (*part, error) {
	if p := s.open[cat]; p != nil {
		return p, nil
	}
	n := s.nextNumber(cat)
	tmp := filepath.Join(s.root, fmt.Sprintf("%s-%04d.zip.tmp", cat, n))
	f, err := s.out.Create(tmp)
	if err != nil {
		return nil, err
	}
	cw := &countWriter{w: f}
	zw := zip.NewWriter(cw)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	p := &part{n: n, tmp: tmp, f: f, cw: cw, zw: zw}
	s.open[cat] = p
	return p, nil
}

func (s *zipSink) Put(ctx context.Context, dest string, mtime time.Time, write func(io.Writer) (salvage.Result, error),
	accept func(salvage.Result) bool) (string, salvage.Result, error) {
	cat, name, ok := strings.Cut(dest, "/")
	if !ok {
		cat, name = "Other", dest
	}
	if err := s.out.MkdirAll(s.root); err != nil {
		return "", salvage.Result{}, err
	}
	spool := filepath.Join(s.root, spoolName)
	if err := removeIfExists(s.out, spool); err != nil {
		return "", salvage.Result{}, err
	}
	f, err := s.out.Create(spool)
	if err != nil {
		return "", salvage.Result{}, err
	}
	defer s.out.Remove(spool)
	res, err := write(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", res, err
	}
	if sum, err := hashFile(spool); err != nil || sum != res.Sum {
		return "", res, ErrVerify
	}
	if !accept(res) {
		return "", res, nil
	}
	if res.Bad > 0 {
		name = PartialName(name)
	}
	p, err := s.partFor(cat)
	if err != nil {
		return "", res, err
	}
	for _, e := range p.entries { // the same file again (a retry): never two entries with one name in a part
		if e.name == name {
			if err := s.closePart(ctx, cat); err != nil {
				return "", res, err
			}
			if p, err = s.partFor(cat); err != nil {
				return "", res, err
			}
			break
		}
	}
	src, err := os.Open(spool)
	if err != nil {
		return "", res, err
	}
	defer src.Close()
	// a file that would push this part past MaxPartBytes starts a new part (FAT32: a part must
	// never be "1 GiB + one big file")
	if st, err := src.Stat(); err == nil && len(p.entries) > 0 && p.cw.n+st.Size() > MaxPartBytes {
		if err := s.closePart(ctx, cat); err != nil {
			return "", res, err
		}
		if p, err = s.partFor(cat); err != nil {
			return "", res, err
		}
	}
	method := zip.Deflate
	sample := make([]byte, sampleBytes)
	n, _ := io.ReadFull(src, sample)
	if Ratio(strings.NewReader(string(sample[:n]))) >= storeBelow {
		method = zip.Store
	}
	src.Seek(0, io.SeekStart)
	w, err := p.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: mtime})
	if err != nil {
		s.dropBroken(cat)
		return "", res, err
	}
	size, err := io.Copy(w, src)
	if err != nil {
		s.dropBroken(cat)
		return "", res, err
	}
	stored := fmt.Sprintf("%s-%04d.zip/%s", cat, p.n, name)
	p.entries = append(p.entries, pending{stored: stored, name: name, size: size, sum: res.Sum})
	if len(p.entries) >= MaxEntries || p.cw.n >= MaxPartBytes {
		if err := s.closePart(ctx, cat); err != nil {
			return stored, res, err
		}
	}
	return stored, res, nil
}

// dropBroken drops a part a write failed in: its writer keeps that error forever, so nothing more can
// go into it. Its earlier files are reported failed (the next run copies them again); the next
// file starts a new part.
func (s *zipSink) dropBroken(cat string) {
	p := s.open[cat]
	if p == nil {
		return
	}
	delete(s.open, cat)
	p.f.Close()
	s.out.Remove(p.tmp)
	for _, e := range p.entries {
		s.failed = append(s.failed, e.stored)
	}
}

// closePart finishes a part, re-reads every entry against its hash, renames it into place and
// indexes its entries. A part that fails verification is discarded and its entries reported.
func (s *zipSink) closePart(ctx context.Context, cat string) error {
	p := s.open[cat]
	delete(s.open, cat)
	err := p.zw.Close()
	if err == nil {
		err = p.f.Sync()
	}
	if cerr := p.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = verifyPart(p)
	}
	if err != nil {
		s.out.Remove(p.tmp)
		for _, e := range p.entries {
			s.failed = append(s.failed, e.stored)
		}
		return fmt.Errorf("zip part %s: %w", filepath.Base(p.tmp), err)
	}
	final := strings.TrimSuffix(p.tmp, ".tmp")
	if err := destfs.RenameRetry(ctx, s.out, p.tmp, final); err != nil {
		return err
	}
	s.out.SyncDir(s.root)
	for _, e := range p.entries {
		if err := job.Put(s.st, indexBucket, e.stored, e.size); err != nil {
			return err
		}
	}
	return nil
}

func verifyPart(p *part) error {
	zr, err := zip.OpenReader(p.tmp)
	if err != nil {
		return err
	}
	defer zr.Close()
	got := map[string][32]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return err
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		got[f.Name] = sum
	}
	for _, e := range p.entries {
		if got[e.name] != e.sum {
			return fmt.Errorf("%s: %w", e.name, ErrVerify)
		}
	}
	return nil
}

func (s *zipSink) Close() error {
	var errs []error
	for cat := range s.open {
		if err := s.closePart(context.Background(), cat); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// abandon drops open parts without finishing them (simulates a crash in tests).
func (s *zipSink) abandon() {
	for cat, p := range s.open {
		p.f.Close()
		delete(s.open, cat)
	}
}

// Ratio is compressed/original size of r at Deflate level 9 (1.0 = incompressible).
func Ratio(r io.Reader) float64 {
	cw := &countWriter{w: io.Discard}
	fw, _ := flate.NewWriter(cw, flate.BestCompression)
	n, _ := io.Copy(fw, r)
	fw.Close()
	if n == 0 {
		return 1
	}
	return float64(cw.n) / float64(n)
}
