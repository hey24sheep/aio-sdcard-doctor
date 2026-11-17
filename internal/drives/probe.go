package drives

import (
	"sync"
	"time"
)

// probeTimeout is how long one volume or disk may take to say what it is when drives are listed:
// a dying card can take minutes to answer, or never (SA-7).
const probeTimeout = 5 * time.Second

type pendingProbe[T any] struct {
	done chan struct{}
	v    T
	ok   bool
}

var (
	probesMu sync.Mutex
	probes   = map[string]any{} // key → *pendingProbe[T]
)

// probeWithin runs probe (the OS calls describing one volume or disk) with a deadline: answered
// is false when it didn't answer in time, ok is probe's own (false: nothing there). One still
// stuck from an earlier listing isn't probed again: that listing's probe is waited for instead,
// so a refresh every few seconds can't pile up stuck calls.
func probeWithin[T any](key string, timeout time.Duration, probe func() (T, bool)) (v T, ok, answered bool) {
	probesMu.Lock()
	p, _ := probes[key].(*pendingProbe[T])
	if p == nil {
		p = &pendingProbe[T]{done: make(chan struct{})}
		probes[key] = p
		go func() {
			p.v, p.ok = probe()
			probesMu.Lock()
			delete(probes, key)
			probesMu.Unlock()
			close(p.done)
		}()
	}
	probesMu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return p.v, p.ok, true
	case <-t.C:
		return v, false, false
	}
}

// notResponding is how a volume that didn't answer is listed: its file system can't be read
// right now, so only reading the card itself (image, recover) is offered.
func notResponding(root, device string) Drive {
	return Drive{Root: root, Device: device, FS: notRespondingFS, RAW: true}
}

const notRespondingFS = "not responding"
