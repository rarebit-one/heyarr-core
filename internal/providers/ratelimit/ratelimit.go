// Package ratelimit is a proactive minimum-interval request gate shared by the
// provider adapters that must stay under a service's rate ceiling (ADR-0085,
// ADR-0087).
//
// It was built for opensubtitles.com's ~5 req/s ceiling and, as ADR-0087
// predicted, gained a second and third user in MusicBrainz (~1 req/s, and a hard
// requirement) and Open Library. Rather than leave it unexported inside one
// adapter, it lives here so every adapter shares one tested implementation.
//
// It is a minimum-interval gate, not a token bucket: these services limit a RATE
// rather than a burst allowance, heyarr's access pattern is one request at a
// time behind a job lease, and spacing successive calls is both all that is
// needed and the simplest thing that cannot drift out of a burst budget. It is
// deliberately PROACTIVE — a backfill can project hundreds of lookups at once,
// and earning a ban and then reacting to it is worse than spacing the requests
// so one is never earned.
//
// The clock and the sleep are injected so a test asserts the spacing it computes
// without a real wall-clock wait (ADR-0017's determinism, applied to a limiter).
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// RateLimiter spaces successive requests to stay under a service's rate ceiling.
type RateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

// New builds a limiter that admits at most one request per interval. now and
// sleep are injected for tests; nil means the production wall clock and a
// context-aware sleep. An interval of zero or less disables the limiter (Wait
// returns immediately), which is how a test that is not asserting spacing opts
// out.
func New(
	interval time.Duration,
	now func() time.Time,
	sleep func(ctx context.Context, d time.Duration) error,
) *RateLimiter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if sleep == nil {
		sleep = SleepContext
	}
	return &RateLimiter{interval: interval, now: now, sleep: sleep}
}

// Wait blocks until the next request is allowed, or ctx ends first.
//
// A slot is claimed under the lock — max(now, lastSlot+interval) — and the lock
// is released BEFORE sleeping, so two concurrent callers take sequential slots
// (t, t+interval, …) and each waits for its own without the lock being held
// across a sleep. A cancelled context returns its error rather than sitting in
// the limiter, so a killed job does not linger.
func (r *RateLimiter) Wait(ctx context.Context) error {
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

// SleepContext sleeps for d, returning early with the context's error if it ends
// first. The production sleep behind the limiter.
func SleepContext(ctx context.Context, d time.Duration) error {
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
