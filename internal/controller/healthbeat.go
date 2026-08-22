package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The provider health beat (#164) — the thing that decides when to look at the
// providers.
//
// # The bug this closes
//
// providers.HealthJobType was declared, its handler was registered, and
// NOTHING enqueued it. Reconciliation and the upgrade scan were the whole of
// the scheduled work and neither touches provider health, so on a running node
// every provider reported "not checked yet" forever.
//
// The consequence was worse than a stale field. Every assertion anywhere that
// read provider health was asserting on a value nothing ever wrote, so it held
// unfalsifiably — and an unfalsifiable assertion reads as coverage. This was
// found by a sabotage to the indexer client's error path FAILING TO FIRE: the
// test was fine and the mechanism underneath it was absent.
//
// # Why the controller owns it
//
// Deciding that work should happen is control-plane (§7); doing it is a
// worker's (§9). So the controller enqueues and a worker runs, which is also
// the only way the two are allowed to talk (invariant 4, ADR-0002) — the
// worker may not even be the same process, which is precisely why a ticker
// inside whatever happens to hold the registry would not do.
//
// # Why this is a fourth beat and not a scheduler
//
// reconcile.go has said since M3-05 that generalising before three users exist
// is how a scheduler grows features nobody asked for. This is the shape those
// two beats already have — one sweep, one interval, one dedupe key — so it is
// nine lines that differ only in interval and job type, exactly as the upgrade
// scan was. The beat that does NOT have this shape is the search beat (#130):
// per-want questions, two cadences, a per-want backoff and an external hold-off
// condition. A common abstraction over all four would be a cron table plus
// three special cases.

// providerHealthInterval is how often every configured provider is exercised.
//
// # One minute, and why that does not contradict #130
//
// #130 sets an HOUR as the floor for searching one want, on the grounds that
// the thing that breaks under an impatient scheduler is somebody's tracker
// ACCOUNT, which a restart does not restore. That reasoning is not weakened
// here; it simply prices a different request.
//
//   - A search pass costs one request PER DUE WANT per indexer. A library of
//     four hundred wants is four hundred requests, and the cost grows with the
//     library — which is what makes an hour the right floor there.
//   - A health pass costs one request PER CONFIGURED PROVIDER, and a homelab
//     has a handful. That cost is FIXED: it is the same on a library of four
//     hundred wants as on an empty one, forever.
//
// The request is different too. A health check is the `t=caps` handshake (#99,
// ADR-0028), which the indexer application answers about itself; a search is
// what it proxies to a tracker. Asking Jackett what it can do is not asking a
// tracker for anything.
//
// # The deciding argument is the capabilities cache (#131)
//
// STATE THIS WHERE SOMEBODY WILL MEET IT, which is here and in
// internal/indexers/client.go's capsTTL: #131 made the health check the
// capabilities cache's INVALIDATION PATH — Check calls refreshCapabilities —
// so this interval silently IS the cache's real refresh rate. capsTTL (ten
// minutes) is documented as a BACKSTOP covering a node whose health job has
// not come round yet, and until this beat existed it was not a backstop at
// all: it was the only mechanism there was.
//
// For the TTL to be the backstop it says it is, the beat has to be an order of
// magnitude faster than it. A minute is that, with room; five minutes would
// leave the backstop firing every other refresh and would make the comment in
// client.go aspirational again.
//
// It is also what an operator reads. Staleness on GET /api/v1/providers is the
// field somebody looks at when acquisitions have stopped, and "checked forty
// seconds ago" is an answer while "checked fifty minutes ago" is another
// question.
//
// A constant rather than configuration, for the reason reconcileInterval is:
// nothing yet suggests an operator would want a different number, and a knob
// that exists because it was easy to add is one somebody eventually sets to
// something harmful.
const providerHealthInterval = time.Minute

// startProviderHealth enqueues a pass now and then on the beat.
//
// The immediate one is the more important of the two. A restart is exactly
// when an operator wants to know what this node can reach — half of all
// restarts are somebody having just changed a provider — and it is also what
// gives them a force-refresh path for the capabilities cache that does not
// involve waiting out a TTL. Reconciliation enqueues at startup for the
// analogous reason; the upgrade scan deliberately does not, because going
// looking for better copies six times during a debugging session is bandwidth,
// and a health check is not.
//
// It runs on a DEGRADED NODE, unconditionally, and that is the same judgement
// the reconciliation registration records at internal/worker/worker.go:203 —
// "a fully degraded node still knows what it is missing, which is exactly the
// node whose operator most needs to be told." The argument is stronger here.
// Reconciliation merely still works on a degraded node; this one is the thing
// that REPORTS the degradation. A health beat that stood down when the
// providers were unreachable would go quiet at the only moment anybody reads
// it. The worker side agrees: the handler is registered with no
// RequiredCapability, so a node with no providers claims the job, finds
// nothing and does nothing.
func startProviderHealth(ctx context.Context, queue *jobs.Queue, log *slog.Logger) {
	enqueue := func(reason string) {
		if err := enqueueProviderHealth(ctx, queue); err != nil {
			// Never fatal, for the same reason reconciliation's is not: this
			// is how Heyarr notices things, not how it works. A node that
			// cannot enqueue a health pass still serves, still plays and still
			// ingests, and the next beat will try again.
			log.Warn("could not enqueue a provider health pass",
				"reason", reason, "error", err)
		}
	}
	enqueue("startup")

	go func() {
		ticker := time.NewTicker(providerHealthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				enqueue("beat")
			}
		}
	}()
	log.Info("provider health beat started", "interval", providerHealthInterval)
}

// enqueueProviderHealth queues one pass over every configured provider.
//
// The dedupe key is providers.HealthDedupeKey — the queue's existing mechanism
// (invariant 9), and the key the providers package already declares as "ONE
// key for the whole pass". It is what makes two ROLES produce one check: in a
// deployment where more than one process runs a controller, or across the
// restart of a single one, a pass already queued or leased is the same pass
// and Enqueue returns it rather than creating a second. It is also what stops
// a slow pass piling up behind itself, which is the failure mode a naive timer
// produces and produces precisely when the providers are already struggling.
//
// There is deliberately no per-provider variant, unlike reconciliation's
// per-want one. The pass is one round trip per provider and the registry is
// the unit an operator reads; a scoped health check would be a second dedupe
// key buying nothing.
func enqueueProviderHealth(ctx context.Context, queue *jobs.Queue) error {
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      providers.HealthJobType,
		DedupeKey: providers.HealthDedupeKey,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing a provider health pass: %w", err)
	}
	return nil
}
