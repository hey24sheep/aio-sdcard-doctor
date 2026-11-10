package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/rpc"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/fsread"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

const (
	// MaxRestarts failures within RestartWindow are tolerated; the next one asks the user (R10).
	MaxRestarts   = 5
	RestartWindow = 10 * time.Minute
	// reapWait is how long a respawn waits for the killed worker to die (its queued I/O drains).
	reapWait = 10 * time.Second
)

// Command builds the worker process (a seam for tests).
var Command = func() *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return exec.Command(exe, "__source-worker")
}

// budget counts failures in a sliding window.
type budget struct {
	now   func() time.Time
	times []time.Time
}

func (b *budget) take() bool {
	if b.now == nil {
		b.now = time.Now
	}
	now := b.now()
	keep := b.times[:0]
	for _, t := range b.times {
		if now.Sub(t) < RestartWindow {
			keep = append(keep, t)
		}
	}
	b.times = keep
	if len(b.times) >= MaxRestarts {
		return false
	}
	b.times = append(b.times, now)
	return true
}

// Client talks to the worker. It implements source.Source; all calls are serialised, so the
// card always sees one reader.
type Client struct {
	mu        sync.Mutex
	spec      Spec
	stall     time.Duration
	p         *proc
	lastReap  chan struct{}
	b         budget
	exhausted bool
	gen       int
	open      OpenReply
	first     drives.Identity // the card first opened; every later open must be the same card
	opened    bool
	stderr    string
	closed    bool
}

// Spawn starts a worker and opens spec. stall is the per-call deadline (directory listings 2×).
func Spawn(spec Spec, stall time.Duration) (*Client, error) {
	c := &Client{spec: spec, stall: stall}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(); err != nil {
		c.closeLocked()
		return nil, err
	}
	return c, nil
}

// start launches a worker and re-opens the spec. Caller holds mu.
func (c *Client) start() error {
	if c.lastReap != nil {
		select {
		case <-c.lastReap:
		case <-time.After(reapWait):
		}
	}
	p, err := startProc()
	if err != nil {
		return err
	}
	c.p = p
	c.gen++
	var r OpenReply
	if err := c.callLocked("W.Open", c.spec, &r, 2*c.stall); err != nil {
		return err
	}
	// After a re-plug or a restart the reader must be looking at the same card (same file-system
	// serial, or the same size when there is none); a stranger is never read from.
	if c.opened {
		a, b := drives.NormalizeSerial(c.first.FSSerial), drives.NormalizeSerial(r.ID.FSSerial)
		if (a != "" && b != "" && a != b) || (a == "" && b == "" && c.first.Size != r.ID.Size) {
			if c.p != nil {
				c.lastReap = c.p.kill()
				c.p = nil
			}
			return source.ErrDifferentCard
		}
	} else {
		c.first, c.opened = r.ID, true
	}
	c.open = r
	return nil
}

// fail kills the current worker after a stall or crash and charges the budget.
func (c *Client) fail(kind error) error {
	if c.p != nil {
		c.stderr = c.p.stderr.String()
		c.lastReap = c.p.kill()
		c.p = nil
	}
	if !c.b.take() {
		c.exhausted = true
		return source.ErrTooManyRestarts
	}
	return kind
}

// call runs one RPC with a deadline, (re)spawning the worker first if needed.
func (c *Client) call(method string, args, reply any, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("worker: client closed")
	}
	if c.p == nil {
		if c.exhausted {
			return source.ErrTooManyRestarts
		}
		if err := c.start(); err != nil {
			return err
		}
	}
	return c.callLocked(method, args, reply, timeout)
}

func (c *Client) callLocked(method string, args, reply any, timeout time.Duration) error {
	call := c.p.rpc.Go(method, args, reply, make(chan *rpc.Call, 1))
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-call.Done:
		var se rpc.ServerError
		if call.Error == nil {
			return nil
		}
		if errors.As(call.Error, &se) {
			return decode(string(se))
		}
		return c.fail(source.ErrWorkerCrashed)
	case <-c.p.stderr.fatal:
		return c.fail(source.ErrWorkerCrashed)
	case <-t.C:
		return c.fail(source.ErrStalled)
	}
}

// decode turns a wire error back into a typed error.
func decode(s string) error {
	switch {
	case strings.HasPrefix(s, kNotExist):
		return fmt.Errorf("%s: %w", strings.TrimPrefix(s, kNotExist), fs.ErrNotExist)
	case strings.HasPrefix(s, kPermission):
		return fmt.Errorf("%s: %w", strings.TrimPrefix(s, kPermission), fs.ErrPermission)
	case strings.HasPrefix(s, kLocked):
		return fmt.Errorf("%s: %w", strings.TrimPrefix(s, kLocked), source.ErrLocked)
	case strings.HasPrefix(s, kUnsupported):
		return fmt.Errorf("%s: %w", strings.TrimPrefix(s, kUnsupported), errors.ErrUnsupported)
	case strings.HasPrefix(s, kNeedsAdmin):
		return fmt.Errorf("%s: %w", strings.TrimPrefix(s, kNeedsAdmin), source.ErrNeedsAdmin)
	}
	return errors.New(s)
}

// Retarget points the client at a new spec (the card came back under another drive letter or
// mount point) and restarts the worker without charging the budget.
func (c *Client) Retarget(spec Spec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spec = spec
	if c.p != nil {
		c.lastReap = c.p.kill()
		c.p = nil
	}
	return c.start()
}

// ResetBudget forgets past failures (the user chose "keep trying").
func (c *Client) ResetBudget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.b.times, c.exhausted = nil, false
}

// Close kills the worker and waits for it to exit.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
	return nil
}

func (c *Client) closeLocked() {
	c.closed = true
	if c.p != nil {
		done := c.p.kill()
		c.p = nil
		select {
		case <-done:
		case <-time.After(reapWait):
		}
	}
}

// Pid is the current worker's process id (0 if none).
func (c *Client) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p == nil {
		return 0
	}
	return c.p.pid
}

// StderrTail is the last output of the current or last crashed worker (shown in the log).
func (c *Client) StderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p != nil {
		return c.p.stderr.String()
	}
	return c.stderr
}

func (c *Client) hook(name string) error { return c.call("W.Hook", name, &struct{}{}, c.stall) }

// Kind, Identity, Volumes and Caps describe what was opened.
func (c *Client) Kind() string              { return c.open.Kind }
func (c *Client) Identity() drives.Identity { return c.open.ID }
func (c *Client) Volumes() []fsread.Volume  { return c.open.Volumes }
func (c *Client) Caps() []job.Capability    { return c.open.Caps }
func (c *Client) ReadError() string         { return c.open.ReadErr }

// ReadDir implements fs.ReadDirFS.
func (c *Client) ReadDir(name string) ([]fs.DirEntry, error) {
	var es []DirEnt
	if err := c.call("W.ReadDir", name, &es, 2*c.stall); err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, len(es))
	for i, e := range es {
		out[i] = ent{e}
	}
	return out, nil
}

// Stat implements fs.StatFS.
func (c *Client) Stat(name string) (fs.FileInfo, error) {
	var e DirEnt
	if err := c.call("W.Stat", name, &e, c.stall); err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return ent{e}, nil
}

// OpenAt opens a file; its ReadAt goes through the worker in ≤ 1 MiB calls.
func (c *Client) OpenAt(name string) (io.ReaderAt, int64, io.Closer, error) {
	f := &remote{c: c, method: "W.OpenFile", arg: name}
	if err := f.open(); err != nil {
		return nil, 0, nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f, f.size, f, nil
}

// OpenDevice opens the whole raw device or image (Spec.Raw) for imaging and deep scan.
func (c *Client) OpenDevice() (io.ReaderAt, int64, io.Closer, error) {
	f := &remote{c: c, method: "W.OpenDevice", arg: struct{}{}}
	if err := f.open(); err != nil {
		return nil, 0, nil, err
	}
	return f, f.size, f, nil
}

// Open implements fs.FS (used by fs.WalkDir / fs.ReadFile).
func (c *Client) Open(name string) (fs.File, error) {
	st, err := c.Stat(name)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		es, err := c.ReadDir(name)
		if err != nil {
			return nil, err
		}
		return &dirFile{info: st, ents: es}, nil
	}
	r, size, cl, err := c.OpenAt(name)
	if err != nil {
		return nil, err
	}
	return &regFile{info: st, sr: io.NewSectionReader(r, 0, size), c: cl}, nil
}

// remote is a file or device handle in the worker. A respawn invalidates worker handles, so it
// re-opens itself when the client's generation changes.
type remote struct {
	c      *Client
	method string
	arg    any
	h, gen int
	size   int64
}

func (f *remote) open() error {
	var r HandleReply
	if err := f.c.call(f.method, f.arg, &r, f.c.stall); err != nil {
		return err
	}
	f.c.mu.Lock()
	f.gen = f.c.gen
	f.c.mu.Unlock()
	f.h, f.size = r.Handle, r.Size
	return nil
}

func (f *remote) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		f.c.mu.Lock()
		stale := f.gen != f.c.gen || f.c.p == nil
		f.c.mu.Unlock()
		if stale {
			if err := f.open(); err != nil {
				return n, err
			}
		}
		want := min(len(p)-n, MaxRead)
		var r ReadReply
		if err := f.c.call("W.ReadAt", ReadArgs{Handle: f.h, Off: off + int64(n), N: want}, &r, f.c.stall); err != nil {
			return n, err
		}
		n += copy(p[n:], r.Data)
		if r.EOF || len(r.Data) < want {
			return n, io.EOF
		}
	}
	return n, nil
}

func (f *remote) Close() error {
	f.c.mu.Lock()
	same := f.gen == f.c.gen && f.c.p != nil && !f.c.closed
	f.c.mu.Unlock()
	if same {
		f.c.call("W.Close", f.h, &struct{}{}, f.c.stall)
	}
	return nil
}

type ent struct{ d DirEnt }

func (e ent) Name() string               { return e.d.Name }
func (e ent) Size() int64                { return e.d.Size }
func (e ent) ModTime() time.Time         { return e.d.MTime }
func (e ent) IsDir() bool                { return e.d.Dir }
func (e ent) Sys() any                   { return nil }
func (e ent) Type() fs.FileMode          { return e.Mode().Type() }
func (e ent) Info() (fs.FileInfo, error) { return e, nil }
func (e ent) Mode() fs.FileMode {
	if e.d.Dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type regFile struct {
	info fs.FileInfo
	sr   *io.SectionReader
	c    io.Closer
}

func (f *regFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *regFile) Read(p []byte) (int, error) { return f.sr.Read(p) }
func (f *regFile) Close() error               { return f.c.Close() }

type dirFile struct {
	info fs.FileInfo
	ents []fs.DirEntry
	pos  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Read([]byte) (int, error)   { return 0, errors.New("is a directory") }
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.ents[d.pos:]
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	d.pos += len(rest)
	return rest, nil
}

var _ source.Source = (*Client)(nil)

// listTimeout: the first page of a list walks the whole file system inside the worker.
func (c *Client) listTimeout(cursor int) time.Duration {
	if cursor == 0 {
		return 30 * c.stall
	}
	return 2 * c.stall
}

// Deleted implements source.Undeleter through the worker, a page at a time.
func (c *Client) Deleted(ctx context.Context, emit func(source.Deleted) error) error {
	for cur := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p DeletedPage
		if err := c.call("W.Deleted", PageArgs{Cursor: cur}, &p, c.listTimeout(cur)); err != nil {
			return err
		}
		for _, d := range p.Items {
			if err := emit(d); err != nil {
				return err
			}
		}
		if p.Done && p.Capped {
			return ErrListCapped
		}
		if p.Done {
			return nil
		}
		cur = p.Next
	}
}

// LiveExtents implements source.LiveMapper through the worker, a page at a time.
func (c *Client) LiveExtents(ctx context.Context, emit func(path string, off, n int64) error) error {
	for cur := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p LivePage
		if err := c.call("W.LiveExtents", PageArgs{Cursor: cur}, &p, c.listTimeout(cur)); err != nil {
			return err
		}
		for _, r := range p.Items {
			if err := emit(r.Path, r.Off, r.N); err != nil {
				return err
			}
		}
		if p.Done && p.Capped {
			return ErrListCapped
		}
		if p.Done {
			return nil
		}
		cur = p.Next
	}
}

// Validate runs validate.Check on a found file inside the worker.
func (c *Client) Validate(ext string, volOff int64, extents [][2]int64, size int64) (bool, string, error) {
	var r ValidateReply
	err := c.call("W.Validate", ValidateArgs{Ext: ext, VolOff: volOff, Extents: extents, Size: size}, &r, 10*c.stall)
	return r.OK, r.Why, err
}
