package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injected, advanceable clock so the limiter's spacing is
// asserted without a real wall-clock wait (ADR-0017).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// The limiter spaces successive requests by its interval. The first request goes
// through immediately (nothing to wait behind); each subsequent one waits one
// interval, computed against the injected clock the sleep advances.
func TestRateLimiterSpacesRequests(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		clk.advance(d) // the wait actually elapses
		return nil
	}
	rl := New(100*time.Millisecond, clk.now, sleep)

	for i := 0; i < 3; i++ {
		if err := rl.Wait(context.Background()); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if len(slept) != 2 {
		t.Fatalf("slept %v, want two waits (the first request does not wait)", slept)
	}
	for i, d := range slept {
		if d != 100*time.Millisecond {
			t.Errorf("wait[%d] = %v, want 100ms", i, d)
		}
	}
}

// A cancelled context is honoured rather than sat out in the limiter.
func TestRateLimiterHonoursContext(t *testing.T) {
	rl := New(time.Hour, nil, nil) // real sleep
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rl.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("wait = %v, want context.Canceled", err)
	}
}

// A non-positive interval disables the limiter — no waits at all.
func TestRateLimiterDisabled(t *testing.T) {
	var slept int
	rl := New(0, nil, func(context.Context, time.Duration) error { slept++; return nil })
	for i := 0; i < 5; i++ {
		if err := rl.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if slept != 0 {
		t.Errorf("slept %d times, want 0 for a disabled limiter", slept)
	}
}
