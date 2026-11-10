package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var errBoom = errors.New("boom")

func TestRetryBounded(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 4, func(error) bool { return true }, func() error { calls++; return errBoom })
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want last error", err)
	}
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 5, func(error) bool { return true }, func() error {
		calls++
		if calls < 3 {
			return errBoom
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 5, func(error) bool { return false }, func() error { calls++; return errBoom })
	if calls != 1 || !errors.Is(err, errBoom) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRetryHonoursCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	err := Do(ctx, 100, func(error) bool { return true }, func() error { calls++; cancel(); return errBoom })
	if time.Since(start) > time.Second || calls != 1 {
		t.Fatalf("calls=%d took %v", calls, time.Since(start))
	}
	if err == nil {
		t.Fatal("want error after cancel")
	}
}

func TestWaitUntilDeadline(t *testing.T) {
	start := time.Now()
	err := WaitUntil(context.Background(), 10*time.Millisecond, 50*time.Millisecond, func() bool { return false })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

func TestWaitUntilTrue(t *testing.T) {
	n := 0
	if err := WaitUntil(context.Background(), time.Millisecond, time.Second, func() bool { n++; return n == 3 }); err != nil {
		t.Fatal(err)
	}
}

func TestWaitUntilCtxCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := WaitUntil(ctx, 5*time.Millisecond, 0, func() bool { return false })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
