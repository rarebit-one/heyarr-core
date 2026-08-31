package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The follow beat (§55, M12) — the thing that decides when to ask a source what
// it now has.
//
// # It is the search beat's sibling, and deliberately so
//
// The search beat decides when to look for a WANT; this decides when to ask a
// SOURCE what items it now emits. They are the same shape — a controller
// enqueues and a worker runs (invariant 4, ADR-0002); a tick asks "what is due"
// on a per-subject backoff; a hold-off skips the pass entirely when nothing
// could answer — so this is searchbeat.go with three nouns changed, and where it
// differs from searchbeat the difference is called out rather than smoothed
// over. The output of a poll is DesiredItems, which the search beat then drives:
// the follow beat feeds the search beat.
//
// # The tick is granularity, not cadence
//
// The pass runs every followBeatInterval and enqueues only the sources that are
// DUE. The cadence a feed host actually feels is a source's next_poll_at — hours,
// per followed.FeedPoll — so shortening the tick makes Heyarr notice a due source
// sooner, not poll anything more often.

// followBeatInterval is how often the pass asks "which sources are due?". Thirty
// seconds, the same as the search beat and for the same reason: the pass is one
// indexed query returning nothing on a resting library, so the tick is bounded
// by how soon a freshly-followed source should first be polled, not by cost.
const followBeatInterval = 30 * time.Second

// followBatchLimit bounds how many polls one pass may enqueue, so a restart that
// finds four hundred sources overdue hands them out across several passes, most
// overdue first, rather than in one burst against a feed host.
const followBatchLimit = 50

// followScheduler enqueues a poll for each source that is due one.
type followScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	clock   searchClock
	log     *slog.Logger

	// heldOff remembers whether the previous pass held off for want of a healthy
	// feed adapter, so the log says so once when it starts and once when it stops
	// rather than twice a minute forever.
	heldOff bool
}

func newFollowScheduler(
	cat *catalog.Catalog, queue *jobs.Queue, clock searchClock, log *slog.Logger,
) *followScheduler {
	if clock == nil {
		clock = wallClock{}
	}
	return &followScheduler{catalog: cat, queue: queue, clock: clock, log: log}
}

// pass enqueues a poll for every source that is due one, returning how many it
// enqueued.
//
// Idempotent twice over (invariant 9), exactly as the search beat's is: the job
// carries followed.PollDedupeKey, so a second pass gets the live job back rather
// than queueing a duplicate; and RecordPollScheduled is a compare-and-set, so the
// loser of a two-controller race does not advance the schedule a second time.
func (s *followScheduler) pass(ctx context.Context) (int, error) {
	now, due, err := s.plan(ctx)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	return s.dispatch(ctx, now, due)
}

// plan is the READ half: what time is it, may we poll at all, and which sources
// are due. Split from dispatch for the same testing reason searchbeat's is — see
// searchScheduler.plan.
func (s *followScheduler) plan(ctx context.Context) (time.Time, []catalog.DueSource, error) {
	if err := ctx.Err(); err != nil {
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

	due, err := s.catalog.DueSources(ctx, now, followBatchLimit)
	if err != nil {
		return now, nil, err
	}
	return now, due, nil
}

// dispatch is the WRITE half: enqueue a poll for each due source and advance its
// schedule so the next tick does not re-enqueue it.
func (s *followScheduler) dispatch(
	ctx context.Context, now time.Time, due []catalog.DueSource,
) (int, error) {
	var enqueued int
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return enqueued, nil
		}

		// The dedupe key is the idempotency, using the queue's existing partial-
		// unique index over live jobs (ADR-0008). Routing is by registration: a
		// worker with no feed adapter never registers poll_source and so never
		// claims it, leaving the job PENDING AND VISIBLE (ADR-0025) — so no
		// RequiredCapability here, mirroring the search beat's enqueue rather than
		// stating the same routing twice.
		if _, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      followed.PollSourceJobType,
			Payload:   followed.PollSourcePayload{SourceID: d.SourceID},
			DedupeKey: followed.PollDedupeKey(d.SourceID),
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return enqueued, nil
			}
			// One source failing to enqueue must not stop the rest. Its schedule
			// is not advanced, so it stays due and the next tick tries again.
			s.log.Warn("could not enqueue a source poll",
				"source_id", d.SourceID, "error", err)
			continue
		}
		enqueued++

		// Advance provisionally, even on a dedupe hit — the same stance the search
		// beat takes: a source that stayed due on every tick while its poll sat
		// unclaimed would be re-enqueued twice a minute forever on a node with no
		// worker. The AUTHORITATIVE next_poll_at, reflecting whether the poll
		// found anything new, is the worker's (RecordPollOutcome); this only keeps
		// the next tick from re-enqueueing before that runs.
		next := d.Schedule.NextAt(now, d.Fruitless, d.SourceID)
		advanced, err := s.catalog.RecordPollScheduled(
			ctx, d.SourceID, d.Schedule, d.Fruitless, now, next)
		if err != nil {
			return enqueued, fmt.Errorf("controller: %w", err)
		}
		if !advanced {
			continue
		}
		if d.FirstEver {
			s.log.Info("polling a followed source for the first time",
				"source_id", d.SourceID, "schedule", d.Schedule.Name, "next_poll_at", next)
		}
	}

	if enqueued > 0 {
		s.log.Info("scheduled source polls", "count", enqueued, "due", len(due))
	}
	return enqueued, nil
}

// holdOff decides whether to skip this pass because nothing could enumerate a
// feed — the exact reasoning searchScheduler.holdOff gives, applied to the
// metadata capability.
//
// Hold off only while at least one metadata provider says unhealthy AND none
// says healthy. A node that has never run a health pass has no rows and polls
// anyway — unknown is not unhealthy, and a fresh install must not be silent. A
// node with no metadata provider configured at all also has no rows: it polls,
// the poll_source job stays pending and visible for want of the capability, and
// the operator can SEE the work waiting (ADR-0025). Holding off does NOT advance
// any source's next_poll_at: an adapter being down is not evidence about any
// source, and every source due during the outage is still due when it returns.
func (s *followScheduler) holdOff(ctx context.Context) (bool, error) {
	health, err := s.catalog.MetadataHealth(ctx)
	if err != nil {
		return false, err
	}
	if len(health) == 0 {
		if s.heldOff {
			s.heldOff = false
		}
		return false, nil
	}
	for _, h := range health {
		if h.Healthy {
			if s.heldOff {
				s.log.Info("a feed adapter is healthy again; polls resume")
				s.heldOff = false
			}
			return false, nil
		}
	}
	if !s.heldOff {
		s.log.Warn("holding off on polling sources: every feed adapter is unhealthy",
			"adapters", len(health))
		s.heldOff = true
	}
	return true, nil
}

// startFollowBeat runs a pass now and then on the beat. The immediate pass
// matters for the same reason the search beat's does — a controller that was
// down for hours has sources that came due in that time — and costs nothing on a
// library with none due, since being due is a stored date rather than a
// consequence of starting.
func startFollowBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) {
	s := newFollowScheduler(cat, queue, wallClock{}, log)
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			log.Warn("a source-poll scheduling pass failed", "reason", reason, "error", err)
		}
	}
	run("startup")

	go func() {
		ticker := time.NewTicker(followBeatInterval)
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
	log.Info("follow beat started",
		"interval", followBeatInterval,
		"poll_cadence", followed.FeedPoll().Base)
}
