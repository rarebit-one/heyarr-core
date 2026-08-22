package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The search beat (#130) — the thing that decides when to look.
//
// # The bug this closes
//
// The search job worked, and nothing enqueued it. A DesiredItem in MISSING
// with no search in flight sat there indefinitely, and — the part that
// actually mattered — looked EXACTLY like a want that was being worked on:
// same phase, same state name, same everything. Heyarr could decide what
// should exist and could not decide when to go and look for it.
//
// # Why the controller owns it, and why it is a third beat rather than a
// general scheduler
//
// Deciding that work should happen is control-plane (§7); doing it is a
// worker's (§9). So the controller enqueues and a worker runs, which is also
// the only way the two are allowed to talk (invariant 4, ADR-0002).
//
// reconcile.go has said since M3-05 that generalising a scheduler before three
// users exist is how one grows features nobody asked for, and that when the
// search beat arrived as the third the shape would be obvious from three
// examples. It is, and the shape says NOT to generalise: this beat is not the
// same shape as the other two. Reconciliation and the upgrade scan each
// enqueue ONE sweep on a fixed interval with one dedupe key. This one asks a
// per-want question, on two different cadences, with a per-want backoff, and
// holds off entirely on an external condition. A common abstraction over those
// would be a cron table plus three special cases.
//
// # The tick is granularity, not cadence
//
// The pass runs every searchBeatInterval and enqueues only what is DUE. The
// cadence an indexer actually experiences is next_search_at, which is the
// policy in internal/domain/acquisition/schedule.go — hours and days, not
// seconds. Shortening the tick makes Heyarr notice a due want sooner; it does
// not make it search anything more often.

// searchBeatInterval is how often the pass asks "what is due?".
//
// Thirty seconds. The pass is one indexed query returning, on a resting
// library, nothing at all — so the tick is bounded by how long a freshly
// wanted item should wait before Heyarr starts looking, and half a minute is
// under the threshold where an operator concludes nothing happened.
//
// It is a constant rather than configuration for the reason reconcileInterval
// is: nothing yet suggests an operator would want a different number, and a
// knob that exists because it was easy to add is one somebody eventually sets
// to something harmful. The number that WOULD be worth exposing is the cadence
// in the Schedule, and that one is deliberately not exposed either — see the
// note there about tracker accounts.
const searchBeatInterval = 30 * time.Second

// searchBatchLimit bounds how many searches one pass may enqueue.
//
// A library where four hundred wants come due in the same tick — an import,
// or a restart after an outage — should hand its indexers four hundred
// searches across several passes rather than in one burst. The rest stay due
// and are taken by the next tick thirty seconds later, in overdue order, so
// nothing is skipped and the most overdue goes first.
const searchBatchLimit = 50

// searchClock is the beat's view of time, injected so the backoff is an
// ordinary unit test rather than a sleep (ADR-0017, and the house rule).
type searchClock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// searchScheduler enqueues searches for the wants that are due one.
type searchScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	clock   searchClock
	log     *slog.Logger

	// heldOff remembers whether the previous pass held off for want of a
	// healthy indexer, so the log says so once when it starts and once when it
	// stops rather than twice a minute forever.
	heldOff bool
}

func newSearchScheduler(
	cat *catalog.Catalog, queue *jobs.Queue, clock searchClock, log *slog.Logger,
) *searchScheduler {
	if clock == nil {
		clock = wallClock{}
	}
	return &searchScheduler{catalog: cat, queue: queue, clock: clock, log: log}
}

// pass enqueues a search for every want that is due one, and returns how many
// it enqueued.
//
// Idempotent by construction, twice over (invariant 9). The job carries
// acquisition.SearchDedupeKey, so a second pass — on this role or on another
// one, concurrently — gets the live job back rather than creating a second;
// and the bookkeeping write is a compare-and-set, so the loser of that race
// does not advance the streak a second time.
func (s *searchScheduler) pass(ctx context.Context) (int, error) {
	now, due, err := s.plan(ctx)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	return s.dispatch(ctx, now, due)
}

// plan is the READ half of a pass: what time is it, may we search at all, and
// which wants are due.
//
// It is separated from dispatch for one reason, and it is a testing reason
// worth stating rather than hiding. The race invariant 9 is about is two roles
// that have BOTH decided the same want is due and are both about to enqueue.
// A test that simply ran two whole passes concurrently would not reliably
// reach that state — the control plane is single-writer (ADR-0003), so the
// second pass's read usually happens after the first pass's write and finds
// nothing due, and the test would then be asserting that SQLite serialises
// writers rather than that the dedupe key works. Splitting the halves lets the
// test put both roles in the state that actually matters.
func (s *searchScheduler) plan(ctx context.Context) (time.Time, []catalog.DueSearch, error) {
	if err := ctx.Err(); err != nil {
		// An ordinary shutdown. Checked before anything is read, so a
		// cancelled pass reports "did nothing" rather than surfacing a
		// context error as a scheduling failure in the log.
		return time.Time{}, nil, nil
	}
	now := s.clock.Now().UTC()

	holding, err := s.holdOff(ctx)
	if err != nil {
		return time.Time{}, nil, err
	}
	if holding {
		return now, nil, nil
	}

	due, err := s.catalog.DueSearches(ctx, now, searchBatchLimit)
	if err != nil {
		return now, nil, err
	}
	return now, due, nil
}

// dispatch is the WRITE half: enqueue a search for each due want and advance
// its schedule.
func (s *searchScheduler) dispatch(
	ctx context.Context, now time.Time, due []catalog.DueSearch,
) (int, error) {
	var enqueued int
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			// An ordinary shutdown mid-pass. What has been enqueued stays
			// enqueued and what has not stays due, which is the whole benefit
			// of the schedule being durable.
			return enqueued, nil
		}

		// The dedupe key is the idempotency, and it is the queue's existing
		// mechanism rather than a new one: a partial-unique index over live
		// jobs (ADR-0008), the same shape the provider health pass uses. Two
		// controllers, or a controller and an operator hitting
		// POST /desired/{id}/search at the same moment, produce ONE search.
		//
		// No RequiredCapability, deliberately, and this MIRRORS the API's own
		// enqueue in resources/candidates.go rather than differing from it.
		// Routing happens at registration: a worker with no indexer never
		// registers search_release and so never claims it, which leaves the
		// job PENDING AND VISIBLE — ADR-0025's whole claim. Setting a
		// capability here as well would be a second, redundant statement of
		// the same routing, and the two would eventually disagree.
		if _, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      acquisition.SearchJobType,
			Payload:   acquisition.SearchPayload{DesiredItemID: d.DesiredItemID},
			DedupeKey: acquisition.SearchDedupeKey(d.DesiredItemID),
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return enqueued, nil
			}
			// One want failing to enqueue must not stop the rest of the pass.
			// Its schedule is not advanced, so it stays due and the next tick
			// tries again.
			s.log.Warn("could not enqueue a search",
				"desired_item_id", d.DesiredItemID, "error", err)
			continue
		}
		enqueued++

		// The schedule advances even when the enqueue was a dedupe hit — a
		// search for this want that is still queued or still running.
		//
		// The alternative, "do not count a pass that found a live job", would
		// leave the want due on every tick for as long as its job sat
		// unclaimed, which on a node with no worker is a no-op re-enqueued
		// twice a minute forever. Advancing means the want walks its ordinary
		// backoff instead, which is the right shape: it is not being searched,
		// and the schedule saying so is honest.
		next := d.Schedule.NextAt(now, d.Fruitless, d.DesiredItemID)
		advanced, err := s.catalog.RecordSearchScheduled(
			ctx, d.DesiredItemID, d.Schedule, d.Fruitless, now, next)
		if err != nil {
			return enqueued, fmt.Errorf("controller: %w", err)
		}
		if !advanced {
			// Another pass got there first. The dedupe key already made sure
			// there is only one search; there is nothing to repair.
			continue
		}
		if d.FirstEver {
			s.log.Info("looking for a want for the first time",
				"desired_item_id", d.DesiredItemID, "schedule", d.Schedule.Name,
				"next_search_at", next)
		}
	}

	if enqueued > 0 {
		s.log.Info("scheduled searches", "count", enqueued, "due", len(due))
	}
	return enqueued, nil
}

// holdOff decides whether to skip this pass because nothing could answer a
// search.
//
// # DECISION: hold off while every known indexer is unhealthy, and do not
// penalise a single want for it
//
// #130 names both horns. Scheduling anyway fills the queue with work that will
// fail; not scheduling means nothing resumes when an indexer comes back. The
// resolution is that only the FIRST horn is real here, because of how the
// pieces below already behave:
//
//   - A search against a down indexer is not a harmless no-op. The handler
//     drives the want into SEARCHING, the provider round trip fails, the want
//     is driven back with a failure detail, and the job retries five times on
//     the queue's backoff before dying. Per want. So an outage across a
//     hundred-want library costs five hundred failed round trips, a hundred
//     dead jobs, and an event log in which every want is thrashing — and the
//     dead jobs then free the dedupe key, so the next pass does it all again.
//
//   - Nothing needs to "resume" when an indexer returns, because the schedule
//     is a due DATE and not a queue. Every want that came due during the
//     outage is still due afterwards, and the very next tick — within thirty
//     seconds of the health pass recording a healthy indexer — schedules them
//     all, most overdue first.
//
// The second horn is only real for a design that consumes its schedule as it
// walks. This one does not, and holding off costs at most one tick of latency
// against an outage that has already lasted longer than that.
//
// It also, deliberately, does NOT advance any want's next_search_at. Nothing
// backs off because of an outage: backoff is for a want whose release does not
// exist, and an indexer being down is not evidence about any want at all.
//
// # Unknown is not unhealthy
//
// Holding off requires at least one indexer row that says unhealthy AND no
// indexer row that says healthy. A node that has never run a health pass has
// no rows, and it schedules — a fresh install must not be silent while it
// waits for a beat nothing has run yet. Likewise a node with no indexer
// configured at all: it schedules, the job stays pending and visible for want
// of the capability, and the operator can SEE the work waiting. That is
// ADR-0025's answer and it is a better failure than silence.
func (s *searchScheduler) holdOff(ctx context.Context) (bool, error) {
	health, err := s.catalog.IndexerHealth(ctx)
	if err != nil {
		return false, err
	}
	if len(health) == 0 {
		// Never checked, or nothing declaring `indexer`. Unknown, not
		// unhealthy — see above.
		if s.heldOff {
			s.heldOff = false
		}
		return false, nil
	}
	for _, h := range health {
		if h.Healthy {
			if s.heldOff {
				s.log.Info("an indexer is healthy again; searches resume")
				s.heldOff = false
			}
			return false, nil
		}
	}
	if !s.heldOff {
		s.log.Warn("holding off on scheduling searches: every indexer is unhealthy",
			"indexers", len(health))
		s.heldOff = true
	}
	return true, nil
}

// startSearchBeat runs a pass now and then on the beat.
//
// The immediate pass matters for the same reason reconciliation's does, and
// more so: a controller that has just started may have been down for hours,
// and every want that came due in that time is waiting. Unlike the upgrade
// SCAN — which is deliberately not run at startup, because a restart is no
// reason to go looking for better copies of things that are fine — this one
// costs nothing on a library with nothing due, since being due is a stored
// date rather than a consequence of starting.
func startSearchBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) {
	s := newSearchScheduler(cat, queue, wallClock{}, log)
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			// Never fatal. The beat is how Heyarr goes looking, not how it
			// works — a node that cannot enqueue a search still serves, still
			// plays and still ingests, and the next tick tries again.
			log.Warn("a search scheduling pass failed", "reason", reason, "error", err)
		}
	}
	run("startup")

	go func() {
		ticker := time.NewTicker(searchBeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run("beat")
			}
		}
	}()
	log.Info("search beat started",
		"interval", searchBeatInterval,
		"missing_cadence", acquisition.MissingSearches().Base,
		"upgrade_cadence", acquisition.UpgradeSearches().Base)
}
