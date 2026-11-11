// Package worker runs every card read in a separate process (aio-sdcard-doctor __source-worker)
// spoken to over net/rpc on a private pipe. A read that stalls on a dying card, or a third-party
// parser that hangs, loops, panics or eats memory, only costs a worker: the client kills it,
// respawns it (bounded by a restart budget) and the app keeps going.
package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/rpc"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/carve"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/ioerr"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/validate"
)

const (
	// MaxRead is the largest single RPC read (bounds memory per call).
	MaxRead = 1 << 20
	// workerMemLimit is the worker's soft heap limit; the OS hard cap is 1 GiB (R9).
	workerMemLimit = 768 << 20
	hardMemLimit   = 1 << 30
	hooksEnv       = "AIO_WORKER_TEST_HOOKS"
)

// Spec is what the worker opens.
type Spec struct {
	From   string // mounted folder, raw device (\\.\E:, /dev/sdb) or image file
	Raw    bool   // parse From with the built-in parsers
	Volume int    // which volume when Raw
}

// DirEnt is a directory entry on the wire.
type DirEnt struct {
	Name  string
	Size  int64
	MTime time.Time
	Dir   bool
	Mode  fs.FileMode // type bits only: a symbolic link or special file is listed, never followed
	Err   string      // its details couldn't be read (possible damage): reported, not dropped
}

// OpenReply describes what was opened.
type OpenReply struct {
	Kind    string
	ID      drives.Identity
	Volumes []fsread.Volume
	Caps    []job.Capability
	ReadErr string // the file system can't be read (the raw device still can)
}

// HandleReply is an opened file or device.
type HandleReply struct {
	Handle int
	Size   int64
}

// ReadArgs asks for N ≤ MaxRead bytes at Off.
type ReadArgs struct {
	Handle int
	Off    int64
	N      int
}

// ReadReply carries the bytes read; EOF reports the end was reached.
type ReadReply struct {
	Data []byte
	EOF  bool
}

// Service is the worker's RPC surface. It has no method that can write.
type Service struct {
	mu                        sync.Mutex
	src                       source.Source
	dev                       io.ReaderAt
	devSize                   int64
	closers                   []io.Closer
	handles                   map[int]handle
	next                      int
	hooks                     bool
	deleted                   []source.Deleted // paging caches (rebuilt when a walk starts at cursor 0)
	live                      []LiveRun
	deletedCapped, liveCapped bool
}

type handle struct {
	r io.ReaderAt
	c io.Closer
}

// Wire error kinds; the client turns them back into typed errors.
const (
	kNotExist    = "notexist|"
	kPermission  = "permission|"
	kNeedsAdmin  = "needsadmin|"
	kPanic       = "parser-panic|"
	kLocked      = "locked|"
	kUnsupported = "unsupported|"
)

func wireErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, source.ErrNeedsAdmin):
		return errors.New(kNeedsAdmin + err.Error())
	case ioerr.IsLocked(err):
		return errors.New(kLocked + err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return errors.New(kNotExist + err.Error())
	case errors.Is(err, fs.ErrPermission):
		return errors.New(kPermission + err.Error())
	}
	return err
}

// catch turns a panic in a handler (a parser bug) into an error; the worker survives.
func catch(err *error) {
	if p := recover(); p != nil {
		*err = fmt.Errorf("%s%v", kPanic, p)
	}
}

// Open opens the card or image described by spec.
func (s *Service) Open(spec Spec, r *OpenReply) (err error) {
	defer catch(&err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !spec.Raw {
		src, err := source.OpenFolder(spec.From)
		if err != nil {
			return wireErr(err)
		}
		s.src = src
		*r = OpenReply{Kind: src.Kind(), ID: src.Identity(), Caps: fsread.Capabilities(nil, src)}
		return nil
	}
	dev, size, c, err := source.OpenRawDevice(spec.From)
	if err != nil {
		return wireErr(err)
	}
	s.dev, s.devSize, s.closers = dev, size, append(s.closers, c)
	vols, err := fsread.Probe(dev, size)
	if err != nil {
		*r = OpenReply{Kind: "unknown", ReadErr: err.Error(),
			ID: drives.Identity{Size: uint64(size), Fingerprint: rawSamples(dev, size)}}
		return nil
	}
	if spec.Volume < 0 || spec.Volume >= len(vols) {
		return fmt.Errorf("volume %d not found (%d on this card)", spec.Volume, len(vols))
	}
	v := vols[spec.Volume]
	*r = OpenReply{Kind: v.FS, Volumes: vols, Caps: fsread.Capabilities(&v, nil),
		ID: drives.Identity{Size: uint64(size), FSSerial: v.Serial, Label: v.Label}}
	if drives.NormalizeSerial(v.Serial) == "" {
		r.ID.Fingerprint = rawSamples(dev, size) // no serial (RAW card): tell same-size cards apart
	}
	src, err := fsread.Open(dev, v)
	if err != nil {
		r.ReadErr = err.Error()
		return nil
	}
	fsread.SetIdentity(src, r.ID)
	s.src = src
	return nil
}

func (s *Service) source() (source.Source, error) {
	if s.src == nil {
		return nil, errors.New("the card's file system can't be read")
	}
	return s.src, nil
}

// ReadDir lists a directory.
func (s *Service) ReadDir(name string, r *[]DirEnt) (err error) {
	defer catch(&err)
	src, err := s.source()
	if err != nil {
		return err
	}
	es, err := src.ReadDir(name)
	if err != nil {
		return wireErr(err)
	}
	out := make([]DirEnt, 0, len(es))
	for _, e := range es {
		typ := e.Type() & fs.ModeType // from the directory entry: a link is not followed to find it
		fi, err := e.Info()
		if err != nil {
			out = append(out, DirEnt{Name: e.Name(), Dir: e.IsDir(), Mode: typ, Err: err.Error()})
			continue
		}
		out = append(out, DirEnt{Name: e.Name(), Size: fi.Size(), MTime: fi.ModTime(), Dir: e.IsDir(), Mode: typ})
	}
	*r = out
	return nil
}

// Stat describes one path.
func (s *Service) Stat(name string, r *DirEnt) (err error) {
	defer catch(&err)
	src, err := s.source()
	if err != nil {
		return err
	}
	fi, err := src.Stat(name)
	if err != nil {
		return wireErr(err)
	}
	*r = DirEnt{Name: fi.Name(), Size: fi.Size(), MTime: fi.ModTime(), Dir: fi.IsDir()}
	return nil
}

func (s *Service) add(r io.ReaderAt, c io.Closer) int {
	if s.handles == nil {
		s.handles = map[int]handle{}
	}
	s.next++
	s.handles[s.next] = handle{r, c}
	return s.next
}

// OpenFile opens a file for reading.
func (s *Service) OpenFile(name string, r *HandleReply) (err error) {
	defer catch(&err)
	src, err := s.source()
	if err != nil {
		return err
	}
	ra, size, c, err := src.OpenAt(name)
	if err != nil {
		return wireErr(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	*r = HandleReply{Handle: s.add(ra, c), Size: size}
	return nil
}

// OpenDevice gives read access to the whole raw device or image (imaging, deep scan).
func (s *Service) OpenDevice(_ struct{}, r *HandleReply) (err error) {
	defer catch(&err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dev == nil {
		return errors.New("not opened as a raw device")
	}
	*r = HandleReply{Handle: s.add(s.dev, io.NopCloser(nil)), Size: s.devSize}
	return nil
}

// ReadAt reads up to MaxRead bytes.
func (s *Service) ReadAt(a ReadArgs, r *ReadReply) (err error) {
	defer catch(&err)
	s.mu.Lock()
	h, ok := s.handles[a.Handle]
	s.mu.Unlock()
	if !ok {
		return errors.New("bad handle")
	}
	if a.N < 0 || a.N > MaxRead {
		return fmt.Errorf("read of %d bytes refused (max %d)", a.N, MaxRead)
	}
	buf := make([]byte, a.N)
	n, err := h.r.ReadAt(buf, a.Off)
	r.Data = buf[:n]
	if err == io.EOF {
		r.EOF = true
		return nil
	}
	return wireErr(err)
}

// Close releases a handle.
func (s *Service) Close(h int, _ *struct{}) (err error) {
	defer catch(&err)
	s.mu.Lock()
	hd, ok := s.handles[h]
	delete(s.handles, h)
	s.mu.Unlock()
	if ok && hd.c != nil {
		hd.c.Close()
	}
	return nil
}

// Ping checks the worker is alive.
func (s *Service) Ping(_ struct{}, _ *struct{}) error { return nil }

// Hook simulates parser misbehaviour; it only works in test builds of the worker.
func (s *Service) Hook(name string, _ *struct{}) (err error) {
	defer catch(&err)
	if !s.hooks {
		return errors.New("hooks disabled")
	}
	switch name {
	case "Hang":
		select {}
	case "Panic":
		panic("test parser panic")
	case "Crash":
		os.Exit(3)
	case "Printf":
		fmt.Printf("library chatter %s\n", strings.Repeat("x", 100_000))
		os.Stdout.Write([]byte("gob-looking garbage \x00\x01\x02\n"))
	case "Stderr":
		os.Stderr.Write([]byte(strings.Repeat("noise ", 1<<20/6)))
	case "Alloc":
		var keep [][]byte
		for i := 0; i < 32; i++ { // 2 GiB, touched so it is really committed
			b := make([]byte, 64<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			keep = append(keep, b)
		}
		_ = keep
	}
	return nil
}

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return nil }

// ServeWorker serves the RPC service on rwc until the parent closes it.
func ServeWorker(rwc io.ReadWriteCloser) error {
	srv := rpc.NewServer()
	if err := srv.RegisterName("W", &Service{hooks: os.Getenv(hooksEnv) == "1"}); err != nil {
		return err
	}
	srv.ServeConn(rwc)
	return nil
}

// Main is the hidden __source-worker command. The original stdout carries RPC only; everything
// libraries print goes to stderr (R8). It exits when the parent closes stdin.
func Main() int {
	rpcOut := os.Stdout
	os.Stdout = os.Stderr
	redirectStdout()
	debug.SetMemoryLimit(workerMemLimit)
	if err := ServeWorker(stdio{os.Stdin, rpcOut}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// pageSize is how many deleted records or live runs one call returns (a variable for tests).
var pageSize = 2000

// maxListed bounds what the worker keeps in memory for paging.
var maxListed = 5_000_000

// PageArgs asks for the page starting at Cursor (0 = start over).
type PageArgs struct{ Cursor int }

// DeletedPage is one page of deleted records.
type DeletedPage struct {
	Items  []source.Deleted
	Next   int
	Done   bool
	Capped bool // the list stopped at maxListed
}

// LiveRun is one run of a live file's data on the volume.
type LiveRun struct {
	Path   string
	Off, N int64
}

// LivePage is one page of live runs.
type LivePage struct {
	Items  []LiveRun
	Next   int
	Done   bool
	Capped bool
}

var errListTooLong = errors.New("too many entries to list")

// ErrListCapped: a listing stopped at its cap; everything up to it was delivered.
var ErrListCapped = errors.New("the list was cut short: too many entries on this card")

func page[T any](all []T, cur int) ([]T, int, bool) {
	cur = min(max(cur, 0), len(all))
	end := min(cur+pageSize, len(all))
	return all[cur:end], end, end == len(all)
}

// Deleted pages through the file system's deleted records (collected once per walk).
func (s *Service) Deleted(a PageArgs, r *DeletedPage) (err error) {
	defer catch(&err)
	src, err := s.source()
	if err != nil {
		return err
	}
	u, ok := src.(source.Undeleter)
	if !ok {
		return kUnsupportedErr()
	}
	if a.Cursor == 0 || s.deleted == nil {
		s.deleted = nil
		err := u.Deleted(context.Background(), func(d source.Deleted) error {
			if len(s.deleted) >= maxListed {
				return errListTooLong
			}
			s.deleted = append(s.deleted, d)
			return nil
		})
		s.deletedCapped = errors.Is(err, errListTooLong)
		if errors.Is(err, errors.ErrUnsupported) {
			return kUnsupportedErr()
		}
		if err != nil && !s.deletedCapped {
			return wireErr(err)
		}
	}
	r.Items, r.Next, r.Done = page(s.deleted, a.Cursor)
	r.Capped = r.Done && s.deletedCapped
	return nil
}

// LiveExtents pages through where live files sit.
func (s *Service) LiveExtents(a PageArgs, r *LivePage) (err error) {
	defer catch(&err)
	src, err := s.source()
	if err != nil {
		return err
	}
	lm, ok := src.(source.LiveMapper)
	if !ok {
		return kUnsupportedErr()
	}
	if a.Cursor == 0 || s.live == nil {
		s.live = nil
		err := lm.LiveExtents(context.Background(), func(p string, off, n int64) error {
			if len(s.live) >= maxListed {
				return errListTooLong
			}
			s.live = append(s.live, LiveRun{Path: p, Off: off, N: n})
			return nil
		})
		s.liveCapped = errors.Is(err, errListTooLong)
		if errors.Is(err, errors.ErrUnsupported) {
			return kUnsupportedErr()
		}
		if err != nil && !s.liveCapped {
			return wireErr(err)
		}
	}
	r.Items, r.Next, r.Done = page(s.live, a.Cursor)
	r.Capped = r.Done && s.liveCapped
	return nil
}

func kUnsupportedErr() error { return errors.New(kUnsupported + "not supported by this file system") }

// ValidateArgs names a found file by its extents on the opened device.
type ValidateArgs struct {
	Ext     string
	VolOff  int64
	Extents [][2]int64
	Size    int64
}

// ValidateReply is validate.Check's answer.
type ValidateReply struct {
	OK  bool
	Why string
}

// Validate checks a found file (deleted or carved) is complete. It decodes untrusted data, so it
// runs here: a decoder that crashes or hangs takes down only the worker.
func (s *Service) Validate(a ValidateArgs, r *ValidateReply) (err error) {
	defer catch(&err)
	s.mu.Lock()
	dev := s.dev
	s.mu.Unlock()
	if dev == nil {
		return errors.New("not opened as a raw device")
	}
	src := source.ExtentReader{Dev: dev, Off: a.VolOff, Extents: a.Extents}
	if r.OK, r.Why = validate.Check(a.Ext, src, a.Size); r.OK {
		note := r.Why
		if r.OK, r.Why = carve.Verify(a.Ext, src, a.Size); r.OK { // the structure walks to its end
			r.Why = note
		}
	}
	return nil
}

// rawSamples fingerprints a card without a file-system serial: hashes of a few fixed 4 KiB
// blocks, so two unreadable cards of the same size are not taken for one. An unreadable block
// is recorded as "" and skipped when comparing.
func rawSamples(r io.ReaderAt, size int64) []drives.FPBlock {
	var out []drives.FPBlock
	seen := map[int64]bool{}
	b := make([]byte, 4096)
	for _, off := range []int64{0, 64 << 10, 1 << 20, 16 << 20, size / 2 &^ 4095} {
		if off+int64(len(b)) > size || seen[off] {
			continue
		}
		seen[off] = true
		h := ""
		if n, err := r.ReadAt(b, off); n == len(b) && (err == nil || errors.Is(err, io.EOF)) {
			h = fmt.Sprintf("%X", sha256.Sum256(b))
		}
		out = append(out, drives.FPBlock{Where: fmt.Sprintf("off:%d", off), SHA256: h})
	}
	return out
}
