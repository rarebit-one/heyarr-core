package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/cover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The cover beat (ADR-0105) — the thing that decides which held books get their
// own file looked inside for a cover. One beat serves both the backlog (a
// library ingested before this existed) and every book ingested after it, so
// there is no separate backfill command to remember to run.
//
// Each book file is looked inside at most once: the job records its outcome per
// blob in cover_extractions, and a file already looked inside — whether it
// yielded a cover or not — never qualifies again. A book whose Work already has
// a shipped or extracted cover is skipped; one with only a fetched (Open
// Library) cover is not, because the file's own cover is preferred.
//
// Controller enqueues, worker runs (invariant 4). A PDF's job requires
// pdftoppm, so on a node without it those jobs wait, visible, while EPUBs
// proceed; the due query skips files whose job is already live, so the waiting
// PDFs never hold the batch.

// coverBeatInterval is how often the pass asks "which book files are due?".
const coverBeatInterval = 60 * time.Second

// coverBatchLimit bounds the jobs one pass enqueues. A pass a minute at this size
// works through a few-thousand-book backlog in an hour or two without flooding
// the queue ahead of everything else.
const coverBatchLimit = 50

type coverScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	log     *slog.Logger
}

// pass enqueues an extract_cover job for each book file due one and returns how
// many it enqueued.
func (s *coverScheduler) pass(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil
	}
	due, err := s.catalog.DueCoverExtractions(ctx, coverBatchLimit)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	var enqueued int
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return enqueued, nil
		}
		if _, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:               cover.JobType,
			Payload:            cover.Payload{BlobHash: d.BlobHash, AssetID: d.AssetID, MIME: d.MIME},
			DedupeKey:          cover.DedupeKey(d.BlobHash),
			RequiredCapability: cover.RequiredCapability(d.MIME),
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return enqueued, nil
			}
			s.log.Warn("could not enqueue a cover extraction", "asset_id", d.AssetID, "error", err)
			continue
		}
		enqueued++
	}
	return enqueued, nil
}

// startCoverBeat runs a pass now and then on the beat, like the enrich beat.
func startCoverBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger, newTicker tickerFunc) {
	s := &coverScheduler{catalog: cat, queue: queue, log: log}
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			log.Warn("a cover scheduling pass failed", "reason", reason, "error", err)
		}
	}
	startBeat(ctx, log, newTicker, beat{name: "cover", interval: coverBeatInterval, startup: true, pass: run})
}
