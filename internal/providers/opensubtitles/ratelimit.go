package opensubtitles

import (
	"context"
	"sync"
	"time"
)

// rateLimiter spaces successive requests to stay under opensubtitles.com's
// ~5 req/s ceiling (ADR-0085). It is a minimum-interval gate, not a token
// bucket: the service's limit is a rate rather than a burst allowance, heyarr's
// access pattern is one request at a time behind a job lease, and spacing
// successive calls is both all that is needed and the simplest thing that cannot
// drift out of a burst budget.
//
// No adapter in internal/providers had one before — the closest prior art,
// internal/indexers, only REACTS to a 429 after the fact. This is deliberately
// PROACTIVE: a followed series' subtitle backfill (ADR-0085 §6) can project
// hundreds of wants at once, and earning a ban and then reacting to it is worse
// than spacing the requests so one is never earned.
//
// The clock and the sleep are injected so a test asserts the spacing it computes
// without a real wall-clock wait (ADR-0017's determinism, applied to a limiter).
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

func newRateLimiter(
	interval time.Duration,
	now func() time.Time,
	sleep func(ctx context.Context, d time.Duration) error,
) *rateLimiter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if sleep == nil {
		sleep = sleepContext
	}
	return &rateLimiter{interval: interval, now: now, sleep: sleep}
}

// wait blocks until the next request is allowed, or ctx ends first.
//
// A slot is claimed under the lock — max(now, lastSlot+interval) — and the lock
// is released BEFORE sleeping, so two concurrent callers take sequential slots
// (t, t+interval, …) and each waits for its own without the lock being held
// across a sleep. A cancelled context returns its error rather than sitting in
// the limiter, so a killed poll does not linger.
func (r *rateLimiter) wait(ctx context.Context) error {
	if r.interval <= 0 {
		return ctx.Err()
	}
	r.mu.Lock()
	now := r.now()
	slot := now
	if !r.last.IsZero() {
		if earliest := r.last.Add(r.interval); earliest.After(now) {
			slot = earliest
		}
	}
	r.last = slot
	r.mu.Unlock()

	if d := slot.Sub(now); d > 0 {
		return r.sleep(ctx, d)
	}
	return ctx.Err()
}

// sleepContext sleeps for d, returning early with the context's error if it ends
// first. The production sleep behind the limiter.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
