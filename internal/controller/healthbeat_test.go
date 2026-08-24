package controller

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The provider health beat (#164), against a real database and a real queue.
//
// These tests exist because the thing that was missing was not a behaviour
// inside a function — it was that NOTHING CALLED ONE. A unit test of an
// enqueue helper would have passed happily throughout the whole period the job
// was never scheduled, which is exactly the vacuity this issue is about. So
// the first test below drives the REAL controller and asserts on the REAL job
// table, and it is the test the sabotage is aimed at.

// healthQueue opens a queue over a fresh migrated database.
func healthQueue(t *testing.T) (*sqlite.DB, *jobs.Queue) {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	return db, queue
}

// countHealthJobs reads the job table directly.
//
// Directly, rather than through the queue, because the queue has no listing
// API and the thing under test is whether a row EXISTS at all — which is the
// claim a caller of Enqueue cannot make about itself.
func countHealthJobs(t *testing.T, reader *sql.DB, states ...string) int {
	t.Helper()
	query := `SELECT count(*) FROM jobs WHERE type = ?`
	args := []any{providers.HealthJobType}
	if len(states) > 0 {
		query += ` AND state IN (?`
		args = append(args, states[0])
		for _, s := range states[1:] {
			query += `, ?`
			args = append(args, s)
		}
		query += `)`
	}
	var n int
	if err := reader.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		// A missing table is "none yet", not a broken test. The wiring test
		// below opens a second connection to a database a controller is still
		// migrating, so the first few polls legitimately arrive before the
		// jobs table exists — and failing there would make this test assert
		// how fast the machine is.
		if strings.Contains(err.Error(), "no such table") {
			return 0
		}
		t.Fatal(err)
	}
	return n
}

// THE WIRING TEST, and the one the sabotage removes the enqueue from.
//
// It runs the actual controller rather than the beat, because the defect #164
// describes is not reachable any other way: providers.HealthJobType was
// declared, its handler was registered, and the gap was that no scheduled work
// ever produced a row. Only a test that watches the job table of a running
// controller can tell that apart from a healthy system.
func TestAControllerEnqueuesAProviderHealthPassAtStartup(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()
	waitForMigratedDatabase(t, cfg.Database.Path)

	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Polled to a deadline rather than slept on: the beat enqueues during
	// startup, and how long startup takes is a property of the machine.
	//
	// AND the controller's own exit is watched while polling, which is not
	// tidiness. `done` was previously never read before this loop, so a
	// controller that failed to START — the bind losing a port race, most
	// obviously — reported here as "nothing schedules the health pass": a
	// claim about scheduling code that is working perfectly, sending the
	// reader to the wrong file (#220). This is #207's shape one layer up,
	// where the precondition is that the process under test came up at all.
	deadline := time.Now().Add(10 * time.Second)
	for countHealthJobs(t, db.Reader()) == 0 {
		select {
		case err := <-done:
			t.Fatalf("the controller exited before it could enqueue anything: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s job was ever enqueued — nothing schedules the health pass",
				providers.HealthJobType)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Pending and claimable, not merely present. A row in a terminal state
	// would satisfy "something wrote a job" while telling a worker nothing.
	if got := countHealthJobs(t, db.Reader(), "pending"); got != 1 {
		t.Errorf("pending %s jobs = %d, want exactly 1", providers.HealthJobType, got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller did not stop within 5s of cancellation")
	}
}

// Invariant 9, on the key the providers package already declares.
//
// Two roles — or one role across a restart, or a beat that ticked while the
// previous pass was still leased — must produce ONE check. The health pass is
// a whole sweep over every provider, so a second concurrent one would write
// what it found while the first was still looking, and the loser would record
// answers that were already stale.
func TestASecondHealthPassIsTheSamePassWhileTheFirstIsLive(t *testing.T) {
	db, queue := healthQueue(t)
	ctx := t.Context()

	if err := enqueueProviderHealth(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err := enqueueProviderHealth(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if got := countHealthJobs(t, db.Reader()); got != 1 {
		t.Fatalf("two enqueues produced %d jobs, want 1 — the dedupe key is not holding", got)
	}

	// Still one while it is LEASED. This is the case that matters: a beat that
	// ticks during a slow pass must not queue a second sweep behind it, which
	// is the failure mode a naive timer produces precisely when the providers
	// are already struggling.
	claimed, err := queue.Claim(ctx, jobs.ClaimOptions{
		Owner: "test-worker", Types: []string{providers.HealthJobType},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueProviderHealth(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if got := countHealthJobs(t, db.Reader()); got != 1 {
		t.Fatalf("enqueueing during a leased pass produced %d jobs, want 1", got)
	}

	// And a NEW one once the first has finished. A dedupe key that suppressed
	// forever would turn the beat into a one-shot, which is the same defect as
	// having no beat at all, arriving one interval later.
	if err := queue.Complete(ctx, claimed.ID, "test-worker"); err != nil {
		t.Fatal(err)
	}
	if err := enqueueProviderHealth(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if got := countHealthJobs(t, db.Reader()); got != 2 {
		t.Fatalf("after a completed pass there are %d jobs, want 2 — the beat has become a one-shot", got)
	}
}

// The key is the providers package's own, not a second one spelled the same.
//
// Asserted because two constants that agree by coincidence are two constants
// that stop agreeing in a rename, and the symptom would be two concurrent
// sweeps rather than a compile error.
func TestTheHealthPassUsesTheDeclaredDedupeKey(t *testing.T) {
	db, queue := healthQueue(t)
	if err := enqueueProviderHealth(t.Context(), queue); err != nil {
		t.Fatal(err)
	}
	var key string
	err := db.Reader().QueryRowContext(t.Context(),
		`SELECT dedupe_key FROM jobs WHERE type = ?`, providers.HealthJobType).Scan(&key)
	if err != nil {
		t.Fatal(err)
	}
	if key != providers.HealthDedupeKey {
		t.Errorf("dedupe key = %q, want %q", key, providers.HealthDedupeKey)
	}
}

// A degraded node checks its providers too (worker.go:203's argument, harder).
//
// No RequiredCapability, so any worker claims it. A health beat that routed to
// nodes with a working indexer would go quiet at the only moment anybody reads
// it — the whole point of the pass is to REPORT that a provider is down.
func TestTheHealthPassIsClaimableByANodeWithNoCapabilities(t *testing.T) {
	_, queue := healthQueue(t)
	ctx := t.Context()
	if err := enqueueProviderHealth(ctx, queue); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(ctx, jobs.ClaimOptions{Owner: "degraded-node"})
	if err != nil {
		t.Fatalf("a node advertising nothing could not claim the health pass: %v", err)
	}
	if claimed.Type != providers.HealthJobType {
		t.Errorf("claimed %q, want %q", claimed.Type, providers.HealthJobType)
	}
}

// The beat stops with its context, and its startup enqueue is not conditional
// on the ticker ever firing.
func TestTheHealthBeatStopsWithItsContext(t *testing.T) {
	db, queue := healthQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	startProviderHealth(ctx, queue, discard())
	if got := countHealthJobs(t, db.Reader()); got != 1 {
		t.Fatalf("starting the beat enqueued %d jobs, want 1 immediately", got)
	}
	cancel()
	// Nothing to assert on a stopped goroutine except that nothing more
	// arrives; the interval is a minute, so a tick within this window would be
	// a bug of a different kind.
	time.Sleep(50 * time.Millisecond)
	if got := countHealthJobs(t, db.Reader()); got != 1 {
		t.Errorf("a cancelled beat kept enqueueing: %d jobs", got)
	}
}

// The interval is a decision, so it is asserted rather than left to drift.
//
// Two bounds, both load-bearing and both explained in healthbeat.go:
//
//   - Faster than indexers.capsTTL by an order of magnitude, because #131 made
//     the health check the capabilities cache's invalidation path. This
//     interval IS that cache's refresh rate, and the ten-minute TTL is only a
//     backstop if the beat comfortably outruns it.
//   - Not faster than a handshake is worth. A pass costs one request per
//     configured provider — a fixed cost, unlike #130's per-want searches —
//     but it is still a request to somebody else's service.
func TestTheHealthBeatIntervalStaysAheadOfTheCapabilitiesCache(t *testing.T) {
	// internal/indexers is not imported here: this package must not depend on
	// an integration to state its own policy, and capsTTL is unexported
	// anyway. The number is restated with the reason it has to match.
	const capsTTL = 10 * time.Minute
	if providerHealthInterval*10 > capsTTL {
		t.Errorf("providerHealthInterval = %s, which does not leave the %s capabilities TTL as a backstop",
			providerHealthInterval, capsTTL)
	}
	if providerHealthInterval < 30*time.Second {
		t.Errorf("providerHealthInterval = %s — faster than a provider handshake is worth",
			providerHealthInterval)
	}
}
