// Package retry holds the only retry and wait loops in the program; both are always bounded.
package retry

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// ErrTimeout is returned by WaitUntil when the deadline passes.
var ErrTimeout = errors.New("timed out waiting")

// Do calls fn until it succeeds, returns an error retryable rejects, maxTries calls were made,
// or ctx is done. Waits grow from 100 ms to at most 2 s. Returns fn's last error.
func Do(ctx context.Context, maxTries uint, retryable func(error) bool, fn func() error) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 100 * time.Millisecond
	b.MaxInterval = 2 * time.Second
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := fn()
		if err != nil && !retryable(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(b), backoff.WithMaxTries(maxTries), backoff.WithMaxElapsedTime(0))
	return err
}

// WaitUntil polls cond every poll until it is true, deadline passes (ErrTimeout) or ctx is done.
// deadline 0 waits until ctx is done.
func WaitUntil(ctx context.Context, poll, deadline time.Duration, cond func() bool) error {
	var expired <-chan time.Time
	if deadline > 0 {
		dt := time.NewTimer(deadline)
		defer dt.Stop()
		expired = dt.C
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-expired:
			return ErrTimeout
		case <-t.C:
		}
	}
}
