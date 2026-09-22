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

// The subtitle fetch beat (ADR-0085) — the thing that decides when to ask a
// provider for a want's subtitle. The direct-route sibling of the search beat.
//
// # Why it is its own beat and not the search beat
//
// A subtitle want is RouteDirect: its bytes come from a subtitle provider, never
// an indexer, so the search beat (which enqueues indexer searches) skips it. But
// something still has to enqueue the fetch, on a cadence that respects the
// provider's hard daily quota. That is this beat: a tick asks the catalog which
// subtitle wants are DUE (in MISSING, their video held, past their fetch
// schedule) and enqueues one fetch_subtitle job each.
//
// # No health hold-off, unlike the search beat
//
// The search beat holds off entirely when every indexer is unhealthy, to avoid a
// pass of pointless searches. This beat does not need to: the fetch job itself
// treats an unreachable or quota-spent provider as "found nothing for now" and
// backs the want off on the fetch schedule, so a provider outage costs at most
// one tick's worth of jobs before every due want is backed off for hours. The
// pacing lives in the job's outcome, not in a pre-check the beat would have to
// keep in step with a separate health read.
//
// Controller enqueues, worker runs (invariant 4): the beat is control-plane, the
// fetch is a worker job registered only where a subtitle provider exists, so on a
// node without one the job stays PENDING AND VISIBLE (ADR-0025).

// subtitleBeatInterval is how often the pass asks "what is due?". Thirty seconds,
// the search beat's tick, and for the same reason: the pass is one indexed query
// returning nothing on a resting library, and the cadence a provider actually
// feels is next_fetch_at (hours), not this.
const subtitleBeatInterval = 30 * time.Second

// subtitleBatchLimit bounds how many fetches one pass may enqueue, so a library
// that has just gained many captioned-needing episodes does not enqueue them all
// at once against a quota.
const subtitleBatchLimit = 50

type subtitleScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	clock   searchClock
	log     *slog.Logger
}

func newSubtitleScheduler(cat *catalog.Catalog, queue *jobs.Queue, clock searchClock, log *slog.Logger) *subtitleScheduler {
	if clock == nil {
		clock = wallClock{}
	}
	return &subtitleScheduler{catalog: cat, queue: queue, clock: clock, log: log}
}

// pass enqueues a fetch for every subtitle want due one, and returns how many it
// enqueued. Idempotent: the job carries FetchSubtitleDedupeKey, so a re-enqueued
// want (still due because its job has not run yet) gets the live job back rather
// than a second. The schedule is advanced by the JOB, not here — a fetch that
// found nothing backs the want off, and until the job runs the dedupe key is what
// keeps the beat from stacking.
func (s *subtitleScheduler) pass(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil
	}
	now := s.clock.Now().UTC()
	due, err := s.catalog.DueSubtitleFetches(ctx, now, subtitleBatchLimit)
	if err != nil || len(due) == 0 {
		return 0, err
	}

	var enqueued int
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return enqueued, nil
		}
		if _, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      acquisition.FetchSubtitleJobType,
			Payload:   acquisition.FetchSubtitlePayload{DesiredItemID: d.DesiredItemID},
			DedupeKey: acquisition.FetchSubtitleDedupeKey(d.DesiredItemID),
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return enqueued, nil
			}
			s.log.Warn("could not enqueue a subtitle fetch",
				"desired_item_id", d.DesiredItemID, "error", err)
			continue
		}
		enqueued++
	}
	return enqueued, nil
}

// startSubtitleBeat runs a pass now and then on the beat, like the search beat.
func startSubtitleBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) {
	s := newSubtitleScheduler(cat, queue, wallClock{}, log)
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			log.Warn("a subtitle fetch scheduling pass failed", "reason", reason, "error", err)
		}
	}
	run("startup")

	go func() {
		ticker := time.NewTicker(subtitleBeatInterval)
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
	log.Info("subtitle fetch beat started", "interval", subtitleBeatInterval)
}
