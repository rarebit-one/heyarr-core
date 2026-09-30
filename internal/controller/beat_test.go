package controller

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// fakeTicker is a beat clock a test drives by hand. Its channel is unbuffered,
// so a send completes only when the beat takes it — and a SECOND send
// completes only once the pass the first one started has returned, which is
// what lets a test assert on a pass's effects without sleeping.
type fakeTicker struct {
	tick     chan time.Time
	stopped  chan struct{}
	interval time.Duration
	made     int
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{tick: make(chan time.Time), stopped: make(chan struct{})}
}

// newTicker is the tickerFunc a beat under test is given.
func (f *fakeTicker) newTicker(interval time.Duration) (<-chan time.Time, func()) {
	f.interval = interval
	f.made++
	return f.tick, func() { close(f.stopped) }
}

// fire delivers one tick, failing the test if the beat is not listening.
func (f *fakeTicker) fire(t *testing.T) {
	t.Helper()
	select {
	case f.tick <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the beat never took a tick while its context was live")
	}
}

// awaitStop waits for the beat to release its ticker, then proves it no
// longer listens on it.
func (f *fakeTicker) awaitStop(t *testing.T) {
	t.Helper()
	select {
	case <-f.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the beat did not stop its ticker after its context was cancelled")
	}
	select {
	case f.tick <- time.Now():
		t.Error("the beat took a tick after its context was cancelled")
	default:
	}
}

// startBeat runs the startup pass before it returns, a "beat" pass per tick,
// and stops its ticker with its context; a beat that opts out of the startup
// pass waits for its first tick.
func TestStartBeat(t *testing.T) {
	for _, startup := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		var mu sync.Mutex
		var reasons []string
		pass := func(reason string) {
			mu.Lock()
			defer mu.Unlock()
			reasons = append(reasons, reason)
		}
		seen := func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(reasons)
		}

		clock := newFakeTicker()
		startBeat(ctx, discard(), clock.newTicker, beat{
			name: "test", interval: time.Minute, startup: startup, pass: pass,
		})
		var want []string
		if startup {
			want = []string{"startup"}
		}
		if got := seen(); !slices.Equal(got, want) {
			t.Fatalf("startup=%v: passes before any tick = %v, want %v", startup, got, want)
		}
		if clock.interval != time.Minute {
			t.Errorf("startup=%v: ticker made at %v, want the beat's interval", startup, clock.interval)
		}

		clock.fire(t)
		clock.fire(t) // returns only once the first tick's pass has finished
		want = append(want, "beat")
		if got := seen(); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
			t.Fatalf("startup=%v: passes after two ticks = %v, want a prefix of %v", startup, got, want)
		}

		cancel()
		clock.awaitStop(t)
	}
}

// Every controller beat runs on the clock it is handed, at its own interval,
// and stops with its context. The job-type rows pin each beat's startup
// decision: reconciliation, provider health and the download poll enqueue at
// startup; the upgrade scan deliberately waits for its first tick.
func TestEveryBeatRunsOnItsInjectedTicker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		start    func(ctx context.Context, h *beatHarness, newTicker tickerFunc)
		// jobType, when set, is counted before and after the first tick.
		jobType               string
		atStartup, afterATick int
	}{
		{
			name: "reconciliation", interval: reconcileInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startReconciliation(ctx, h.queue, nil, discard(), nt)
			},
			jobType: acquisition.ReconcileJobType, atStartup: 1, afterATick: 1,
		},
		{
			name: "upgrade scan", interval: upgradeScanInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startUpgradeScan(ctx, h.queue, discard(), nt)
			},
			jobType: acquisition.UpgradeScanJobType, atStartup: 0, afterATick: 1,
		},
		{
			name: "provider health", interval: providerHealthInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startProviderHealth(ctx, h.queue, discard(), nt)
			},
			jobType: providers.HealthJobType, atStartup: 1, afterATick: 1,
		},
		{
			name: "download poll", interval: downloadPollInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startDownloadPoll(ctx, []providers.Entry{fakeDownloadClient(t.TempDir())}, h.queue, discard(), nt)
			},
			jobType: downloads.PollJobType, atStartup: 1, afterATick: 1,
		},
		{
			name: "search", interval: searchBeatInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startSearchBeat(ctx, h.cat, h.queue, discard(), nt)
			},
		},
		{
			name: "follow", interval: followBeatInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startFollowBeat(ctx, h.cat, h.queue, discard(), nt)
			},
		},
		{
			name: "subtitle", interval: subtitleBeatInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startSubtitleBeat(ctx, h.cat, h.queue, discard(), nt)
			},
		},
		{
			name: "enrich", interval: enrichBeatInterval,
			start: func(ctx context.Context, h *beatHarness, nt tickerFunc) {
				startEnrichBeat(ctx, h.cat, h.queue, discard(), nt)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBeatHarness(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			clock := newFakeTicker()
			tc.start(ctx, h, clock.newTicker)
			if clock.made != 1 || clock.interval != tc.interval {
				t.Fatalf("made %d tickers at %v, want one at %v", clock.made, clock.interval, tc.interval)
			}
			if tc.jobType != "" {
				if got := countJobs(t, h, tc.jobType); got != tc.atStartup {
					t.Errorf("%s jobs at startup = %d, want %d", tc.jobType, got, tc.atStartup)
				}
			}

			clock.fire(t)
			clock.fire(t) // the first tick's pass has returned
			if tc.jobType != "" {
				if got := countJobs(t, h, tc.jobType); got != tc.afterATick {
					t.Errorf("%s jobs after a tick = %d, want %d", tc.jobType, got, tc.afterATick)
				}
			}

			cancel()
			clock.awaitStop(t)
		})
	}
}

func countJobs(t *testing.T, h *beatHarness, jobType string) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM jobs WHERE type = ?`, jobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
