package job

import (
	"sync"
	"time"
)

// Throttle lets one event through per Every.
type Throttle struct {
	Every time.Duration
	mu    sync.Mutex
	last  time.Time
}

// Ok reports whether an event may be emitted now.
func (t *Throttle) Ok() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.last.IsZero() && now.Sub(t.last) < t.Every {
		return false
	}
	t.last = now
	return true
}
