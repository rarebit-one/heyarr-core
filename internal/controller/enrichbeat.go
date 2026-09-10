package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The enrich beat (ADR-0087) — the thing that decides when to ask an enrich
// provider what a held music/book Work IS. The sibling of the subtitle fetch
// beat, for a job over a Work rather than a want.
//
// # Why it is its own beat
//
// Enrichment is not an acquisition: the album's audio and the book's file are
// already held, so there is no want, no search and no download — only a Work
// whose description is being filled in (ADR-0087 §6). Nothing else enqueues that,
// so this beat does: a tick asks the catalog which held Works are under-enriched
// and past their schedule, and enqueues one enrich_work job each.
//
// # No health hold-off
//
// Like the subtitle beat, this does not pre-check provider health: the enrich job
// treats an unreachable provider or a Work no authority knows as "found nothing
// for now" and backs the Work off on the schedule, so an outage costs at most one
// tick's worth of jobs. The pacing lives in the job's outcome, not a pre-check the
// beat would have to keep in step with a health read.
//
// Controller enqueues, worker runs (invariant 4): the beat is control-plane, the
// enrich is a worker job registered only where an enrich provider exists, so on a
// node without one the job stays PENDING AND VISIBLE (ADR-0025).

// enrichBeatInterval is how often the pass asks "what is due?". A minute — enrich
// is a background chore with no quota to race, and the cadence a provider actually
// feels is next_enrich_at (hours to days), not this.
const enrichBeatInterval = 60 * time.Second

// enrichBatchLimit bounds how many enrich jobs one pass may enqueue, so a library
// that just ingested a thousand books does not enqueue them all at once against a
// keyless service that asks to be treated gently.
const enrichBatchLimit = 25

type enrichScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	clock   searchClock
	log     *slog.Logger
}

func newEnrichScheduler(cat *catalog.Catalog, queue *jobs.Queue, clock searchClock, log *slog.Logger) *enrichScheduler {
	if clock == nil {
		clock = wallClock{}
	}
	return &enrichScheduler{catalog: cat, queue: queue, clock: clock, log: log}
}

// pass enqueues an enrich for every held Work due one, and returns how many it
// enqueued. Idempotent: the job carries EnrichWorkDedupeKey, so a re-enqueued Work
// (still due because its job has not run yet) gets the live job back rather than a
// second. The schedule is advanced by the JOB, not here.
func (s *enrichScheduler) pass(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil
	}
	now := s.clock.Now().UTC()
	due, err := s.catalog.DueEnrichWorks(ctx, now, enrichBatchLimit)
	if err != nil || len(due) == 0 {
		return 0, err
	}

	var enqueued int
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return enqueued, nil
		}
		if _, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      acquisition.EnrichWorkJobType,
			Payload:   acquisition.EnrichWorkPayload{WorkID: d.WorkID},
			DedupeKey: acquisition.EnrichWorkDedupeKey(d.WorkID),
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return enqueued, nil
			}
			s.log.Warn("could not enqueue an enrich", "work_id", d.WorkID, "error", err)
			continue
		}
		enqueued++
	}
	return enqueued, nil
}

// startEnrichBeat runs a pass now and then on the beat, like the subtitle beat.
func startEnrichBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) {
	s := newEnrichScheduler(cat, queue, wallClock{}, log)
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			log.Warn("an enrich scheduling pass failed", "reason", reason, "error", err)
		}
	}
	run("startup")

	go func() {
		ticker := time.NewTicker(enrichBeatInterval)
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
	log.Info("enrich beat started", "interval", enrichBeatInterval)
}
