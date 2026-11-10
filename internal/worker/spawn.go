package worker

import (
	"bytes"
	"io"
	"net/rpc"
	"sync"
)

// ringSize is how much worker stderr is kept for the log (R8).
const ringSize = 64 << 10

// ring keeps the last ringSize bytes written to it. It closes fatal when the Go runtime in the
// worker reports a fatal error: at a hard memory cap the runtime can hang after printing it, so
// the client must not wait for the process to exit.
type ring struct {
	mu    sync.Mutex
	buf   []byte
	fatal chan struct{}
	fired bool
}

func newRing() *ring { return &ring{fatal: make(chan struct{})} }

var fatalMark = []byte("fatal error: ")

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tail := len(r.buf)
	r.buf = append(r.buf, p...)
	if !r.fired && bytes.Contains(r.buf[max(0, tail-len(fatalMark)):], fatalMark) {
		r.fired = true
		close(r.fatal)
	}
	if over := len(r.buf) - ringSize; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

type pipe struct {
	io.ReadCloser
	w io.WriteCloser
}

func (p pipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipe) Close() error {
	p.w.Close()
	return p.ReadCloser.Close()
}

// proc is one running worker process.
type proc struct {
	cmd    interface{ Wait() error }
	rpc    *rpc.Client
	stderr *ring
	limits func() // releases OS memory caps / monitors
	once   sync.Once
	done   chan struct{}
	killFn func() error
	pid    int
}

// kill stops the worker and returns a channel closed once the OS has reaped it. The reaper
// goroutine holds no buffers; on a card stuck in a kernel read it may wait until the card
// answers or is unplugged, and the restart budget bounds how many can exist (R10).
func (p *proc) kill() chan struct{} {
	p.once.Do(func() {
		p.killFn()
		p.rpc.Close()
		go func() {
			p.cmd.Wait()
			p.limits()
			close(p.done)
		}()
	})
	return p.done
}
