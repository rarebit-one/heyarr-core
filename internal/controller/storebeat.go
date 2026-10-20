package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The store fetch beat (ADR-0120): the trigger for a game want, which is on the
// store route and so never reaches the indexer search beat.
//
// A pass finds game wants due a fetch, claims each with a compare-and-set on
// its store_fetch_schedule row (so two controllers never start the same
// tens-of-gigabytes fetch twice), records the store's package as the want's
// direct release — the same MISSING → SELECTED walk a podcast enclosure takes —
// and enqueues the grab. From there it is an ordinary acquisition: the GOG
// download client fetches and verifies, and ingest files it under the Work.
//
// The beat only enqueues and writes control-plane rows; the fetch runs on a
// worker with the download capability (invariant 4).

// storeBeatInterval is how often the beat looks. A game want is not urgent the
// way a new episode is; fifteen minutes keeps a fresh want close to immediate
// without a pass every few seconds over a library that is mostly at rest.
const storeBeatInterval = 15 * time.Minute

// storeBatchLimit bounds the fetches one pass starts. A game is tens of
// gigabytes, so a library-wide backfill is drip-fed rather than started all at
// once; the rest are picked up on later passes.
const storeBatchLimit = 3

// strandedGrace is how long a store selection may sit in SELECTED with no grab
// queued before the beat re-queues it. Long enough that a grab the beat itself
// just queued has been leased; short against a fetch that would otherwise never
// start.
const strandedGrace = 10 * time.Minute

type storeScheduler struct {
	catalog *catalog.Catalog
	queue   *jobs.Queue
	clock   searchClock
	log     *slog.Logger
}

func newStoreScheduler(cat *catalog.Catalog, queue *jobs.Queue, clock searchClock, log *slog.Logger) *storeScheduler {
	if clock == nil {
		clock = wallClock{}
	}
	return &storeScheduler{catalog: cat, queue: queue, clock: clock, log: log}
}

// pass starts the store fetches that are due, and reports how many grabs it
// enqueued.
func (s *storeScheduler) pass(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil
	}
	now := s.clock.Now().UTC()

	// First, any store selection whose grab never made it onto the queue: the
	// beat records SELECTED and then enqueues, and a shutdown or a queue error
	// between the two would otherwise leave the want selected forever.
	stranded, err := s.catalog.StrandedStoreSelections(ctx, now.Add(-strandedGrace), storeBatchLimit)
	if err != nil {
		return 0, err
	}
	var started int
	for _, id := range stranded {
		if s.enqueueGrab(ctx, id) {
			started++
		}
	}

	// Recovered grabs and new ones share one bound: a recovery backlog must not
	// double the burst a pass is allowed to start.
	room := storeBatchLimit - started
	if room <= 0 {
		return started, nil
	}
	due, err := s.catalog.DueStoreFetches(ctx, now, room)
	if err != nil || len(due) == 0 {
		return started, err
	}
	schedule := acquisition.StoreFetches()

	// One fetch per game per pass: two wants for the same game (two profiles)
	// share one transfer, and its ingest satisfies both.
	games := map[string]bool{}
	for _, d := range due {
		if games[d.WorkID] {
			continue
		}
		games[d.WorkID] = true
		if err := ctx.Err(); err != nil {
			return started, nil
		}
		if !acquisition.ValidGOGGameName(d.GameName) {
			// A malformed external id would be refused by the client anyway;
			// back it off at the ceiling rather than retrying it every pass.
			s.log.Warn("a game's GOG id is not a valid gamename; not fetching it",
				"desired_item_id", d.DesiredItemID)
			_, _ = s.catalog.RecordStoreFetchAttempt(ctx, d.DesiredItemID, d.Fruitless+1, now, now.Add(schedule.Max))
			continue
		}
		fruitless := d.Fruitless + 1
		won, err := s.catalog.RecordStoreFetchAttempt(ctx, d.DesiredItemID, fruitless, now,
			now.Add(schedule.Delay(fruitless-1)))
		if err != nil {
			return started, err
		}
		if !won {
			continue // another pass claimed it
		}
		selected, err := s.catalog.RecordDirectRelease(ctx, d.DesiredItemID,
			acquisition.StoreReleaseCandidate(d.GameName, d.Title, d.DesiredItemID))
		if err != nil {
			return started, err
		}
		if !selected {
			continue // the want moved on since the due query; nothing to grab
		}
		if s.enqueueGrab(ctx, d.DesiredItemID) {
			started++
		}
	}
	return started, nil
}

// enqueueGrab queues the grab for a selected store release. A failure is only
// logged: the selection is durable, and StrandedStoreSelections finds it again
// on a later pass.
func (s *storeScheduler) enqueueGrab(ctx context.Context, desiredItemID string) bool {
	_, err := s.queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type: acquisition.GrabJobType,
		Payload: acquisition.GrabPayload{
			DesiredItemID: desiredItemID,
		},
		DedupeKey:          acquisition.GrabDedupeKey(desiredItemID),
		RequiredCapability: providers.CapabilityDownload.JobCapability(),
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Warn("could not enqueue the grab for a store fetch; a later pass retries it",
				"desired_item_id", desiredItemID, "error", err)
		}
		return false
	}
	return true
}

func startStoreBeat(ctx context.Context, cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger, newTicker tickerFunc) {
	s := newStoreScheduler(cat, queue, wallClock{}, log)
	run := func(reason string) {
		if _, err := s.pass(ctx); err != nil && ctx.Err() == nil {
			log.Warn("a store fetch scheduling pass failed", "reason", reason, "error", err)
		}
	}
	startBeat(ctx, log, newTicker, beat{name: "store fetch", interval: storeBeatInterval, startup: true, pass: run})
}
