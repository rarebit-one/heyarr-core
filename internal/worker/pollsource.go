package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The poll_source job (§55, M12) — one feed round-trip for one followed source.
//
// # It enumerates and projects, and archives nothing itself
//
// This is the follow pipeline's whole seam: it asks the source's feed adapter
// what items it now has, records each as a byte-less Item (ADR-0056), and
// projects each new item onto an item-scoped DesiredItem. From there the EXISTING
// acquisition pipeline — the search beat, the grab, the ingest — archives it
// untouched, which is the point of ADR-0057: following = enumerate + project +
// get out of the way. Nothing here searches, grabs or ingests.
//
// # A poll that finds nothing new is not a failure
//
// Like the search job, an empty poll is a modelled outcome, not an error: the
// source is between seasons, and it will not emit sooner because Heyarr asked
// again. The backoff lives on the SCHEDULE (RecordPollOutcome), not on the job's
// retry — a failed enumeration retries on the queue's backoff, a fruitless one
// backs off on the feed-poll cadence.
//
// # Idempotent (invariant 9)
//
// It will be re-run. UpsertItem is keyed on the source-stable item key, so a
// re-poll re-presents the same items without duplicating rows; and
// CreateDesiredItem is refused on a duplicate (target, profile), which the loop
// treats as "already projected" rather than an error. So a poll that crashed
// after some items but before others completes cleanly on its next run.

// PollSourceHandler runs one source's poll. reg resolves the feed adapter, cat
// stores items and wants, and grabs is the queue a fresh want's reconciliation
// is enqueued to (nil-tolerant, so a poll is exercisable without one).
func PollSourceHandler(
	reg *providers.Registry, cat *catalog.Catalog, grabs *jobs.Queue, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload followed.PollSourcePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: poll_source payload is not decodable: %w", err)
		}
		if payload.SourceID == "" {
			return errors.New("worker: poll_source needs a source")
		}

		src, err := cat.FollowSource(ctx, payload.SourceID)
		if err != nil {
			// The source was unfollowed between the beat enqueueing this and the
			// worker claiming it. Nothing to poll; the job is done.
			if errors.Is(err, catalog.ErrNoFollowSource) {
				log.Info("skipping a poll: the source is gone", "source_id", payload.SourceID)
				return nil
			}
			return err
		}

		provider, err := feedProviderFor(reg)
		if err != nil {
			// No feed adapter at all should be unreachable — the job is capability-
			// routed on `metadata`, so a node with none never claims it (ADR-0025).
			// If it happens anyway, fail so the queue retries rather than silently
			// recording a poll that never looked.
			return fmt.Errorf("worker: polling %s: %w", payload.SourceID, err)
		}

		items, err := provider.Enumerate(ctx, src.FeedRef)
		if err != nil {
			// A failed enumeration is a call failure, not an empty feed (see
			// providers.FeedProvider). Fail the job so it retries on the queue's
			// backoff, and do NOT advance the poll schedule — a poll that did not
			// happen must not count as one that found nothing.
			return fmt.Errorf("worker: enumerating source %s: %w", payload.SourceID, err)
		}

		now := time.Now().UTC()
		var discovered, projected int
		for _, fi := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			item, created, err := cat.UpsertItem(ctx, src.WorkID, fi)
			if err != nil {
				return fmt.Errorf("worker: recording an item for source %s: %w", payload.SourceID, err)
			}
			if created {
				discovered++
			}
			if !shouldProject(src, fi) {
				continue
			}
			want := src.ProjectWant(item.ID)
			if _, err := cat.CreateDesiredItem(ctx, want); err != nil {
				if catalog.IsDuplicateWant(err) {
					// Already projected on a previous poll. Idempotent: nothing to do.
					continue
				}
				return fmt.Errorf("worker: projecting a want for source %s: %w",
					payload.SourceID, err)
			}
			projected++
			enqueuePollReconcile(ctx, grabs, want.ID, log)
		}

		// The authoritative schedule: a poll that discovered a new item resets the
		// cadence to its floor, one that did not backs it off. "Discovered" is a
		// NEW Item row, not a projected want — a from_now source's back-catalogue
		// is discovered on the first poll without being projected, and that is
		// still the feed emitting content the cadence should track.
		if err := cat.RecordPollOutcome(ctx, payload.SourceID, discovered > 0, now); err != nil {
			return fmt.Errorf("worker: recording the poll outcome for %s: %w", payload.SourceID, err)
		}

		log.Info("polled a followed source",
			"source_id", payload.SourceID, "items", len(items),
			"discovered", discovered, "projected", projected)
		return nil
	}
}

// feedProviderFor resolves the metadata provider a source's feed is enumerated
// through.
//
// # Phase 1 routes to the single configured metadata provider
//
// A source's Type (tv_series) is what SHOULD choose the adapter, but providers
// declare a capability, not which source types they serve, and Phase 1 ships one
// metadata implementation (TVDB). So this takes the first CapabilityMetadata
// provider in routing order. Routing a source type to one of several metadata
// providers is a later-phase concern — declared here as the seam it will fill,
// not smuggled in early — and until then a deployment configures one feed
// adapter, exactly as it configures its indexers.
func feedProviderFor(reg *providers.Registry) (providers.FeedProvider, error) {
	feeds := reg.FeedProviders()
	if len(feeds) == 0 {
		return nil, fmt.Errorf("%w: %s", providers.ErrNoProvider, providers.CapabilityMetadata)
	}
	return feeds[0], nil
}

// shouldProject decides whether an enumerated item becomes a want now, honouring
// the source's backfill policy. full projects the whole catalogue; from_now
// projects only what the source emitted after it was followed — an item with no
// publish date cannot be shown to be after, so from_now leaves it for a later
// poll rather than backfilling an undated catalogue. Projection itself is
// idempotent, so re-projecting an already-wanted item is a no-op.
func shouldProject(src catalog.StoredSource, fi followed.FeedItem) bool {
	if src.Backfill == followed.BackfillFull {
		return true
	}
	if fi.PublishedAt.IsZero() {
		return false
	}
	return !fi.PublishedAt.Before(src.CreatedAt)
}

// enqueuePollReconcile kicks a fresh want's reconciliation now rather than
// waiting for the search beat, the same immediacy WantContent gives an operator's
// want. Best-effort: the beat picks it up regardless, so a briefly-unavailable
// queue costs latency, not correctness.
func enqueuePollReconcile(ctx context.Context, queue *jobs.Queue, desiredItemID string, log *slog.Logger) {
	if queue == nil {
		return
	}
	if _, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.ReconcileJobType,
		Payload:   acquisition.ReconcilePayload{DesiredItemID: desiredItemID},
		DedupeKey: acquisition.ReconcileDedupeKey + ":" + desiredItemID,
	}); err != nil {
		log.Warn("could not enqueue reconciliation for a projected want",
			"desired_item_id", desiredItemID, "error", err)
	}
}
