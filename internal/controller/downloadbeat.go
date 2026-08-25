package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The download poll beat (#247) — the thing that decides when to ask a download
// client what it has finished.
//
// # The bug this closes, which is the one #164 already closed once
//
// `downloads.PollJobType` was declared, its handler was registered in
// internal/worker/worker.go, `downloads.PollDedupeKey` was declared beside it,
// and NOTHING ENQUEUED IT. The job type's own doc comment says where it was
// meant to come from —
//
//	Declared in this package rather than in the worker so the CONTROLLER can
//	enqueue it without importing the worker
//
// — so the intent was written down and the beat was never built.
//
// The consequence is that a transfer handed to a download client was never
// asked about again. `SELECTED → QUEUED` worked as of #225; `QUEUED →
// downloaded → ingest` could not happen on a running node at all, because the
// only thing that observes a transfer's progress is this pass. §65 keeps
// several ways for bytes to arrive and the ordinary polled one was not among
// them.
//
// This is the SAME DEFECT healthbeat.go records, in the same shape: a declared
// job type, a registered handler, and no enqueuer. That one was found because a
// sabotage failed to fire; this one was found by asking what in `make demo`
// exercises the polled acquisition arc, and the answer was nothing (#247).
// Two instances is a pattern, which is why scripts/claims.list now exists.
//
// # Why the controller owns it
//
// Deciding that work should happen is control-plane (§7); doing it is a
// worker's (§9). So the controller enqueues and a worker runs, which is also
// the only way the two are allowed to talk (invariant 4, ADR-0002) — the worker
// may not be the same process, which is why a ticker inside whatever happens to
// hold the provider registry would not do.

// downloadPollInterval is how often every configured download client is asked
// what it is doing.
//
// # Fifteen seconds, and why that is not the health beat's minute
//
// The two beats price different requests, the way healthbeat.go argues its
// minute is not the search beat's hour.
//
//   - A health pass is a `t=caps` handshake against an INDEXER, which is a
//     remote service that proxies to somebody's tracker. The politeness budget
//     there is real.
//   - A download poll is one RPC to a daemon that is, in every deployment this
//     targets, on the same machine or the same LAN. Its cost is fixed at one
//     request per configured client and does not grow with the library.
//
// The number that actually matters is not the cost, though. **This interval IS
// the latency between a download finishing and Heyarr starting to ingest it**,
// and that is the one place in the acquisition pipeline where a person is
// waiting for something they asked for. A minute of it is a minute of a
// finished file sitting in a directory while nothing happens, which reads as a
// broken pipeline rather than as a cadence.
//
// Fifteen seconds is short enough that the wait is not noticed and long enough
// that a client with a hundred transfers is not re-enumerated four times a
// minute. It is a constant rather than configuration for the reason
// providerHealthInterval and reconcileInterval are: nothing yet suggests an
// operator would want a different number, and a knob that exists because it was
// easy to add is one somebody eventually sets to something harmful.
const downloadPollInterval = 15 * time.Second

// startDownloadPoll enqueues a pass now and then on the beat, when this node
// has a download client at all.
//
// # Why the startup pass is the important one
//
// A restart is exactly when a transfer has most likely finished unobserved.
// Heyarr holds no connection to the client and learns nothing through a
// callback (§58, §61), so everything that completed while this process was down
// is invisible until something asks — and asking is this pass. Without the
// immediate enqueue, a node restarted after an overnight download would sit for
// a full interval with the bytes already on disk.
//
// # Why this one IS conditional, where the health beat is not
//
// healthbeat.go runs unconditionally and says why: it is the thing that reports
// degradation, so standing down when the providers are unreachable would go
// quiet at the only moment anybody reads it. Its handler is registered with no
// RequiredCapability, so a node with no providers claims the job, finds nothing
// and does nothing.
//
// The poll handler is not registered that way. internal/worker/worker.go gives
// it `RequiredCapability: providers.CapabilityDownload.JobCapability()`, so on
// a node with no download client NOTHING CAN CLAIM IT — and an enqueued job
// nothing can claim does not fail, it waits. Beating unconditionally would
// leave one perpetually queued job on every node that has no download client,
// which is a permanent unexplained row in `GET /api/v1/jobs` and exactly the
// kind of thing an operator learns to ignore.
//
// So the beat asks the configuration first. That is a decision about whether
// there is work, not about whether the work can be done — the capability
// routing still decides the latter, and still would if a client were added to
// the registry by some future path this does not know about.
func startDownloadPoll(ctx context.Context, cfg []providers.Entry, queue *jobs.Queue, log *slog.Logger) {
	if !hasDownloadClient(cfg, log) {
		// Said at info rather than debug. "Why is nothing being acquired" is a
		// question this line answers, and the worker's own startup log makes
		// the same statement from the other end by listing the job types it
		// will claim.
		log.Info("no download client is configured, so no download poll beat")
		return
	}

	enqueue := func(reason string) {
		if err := enqueueDownloadPoll(ctx, queue); err != nil {
			// Never fatal, for the same reason the health beat's is not: this
			// is how Heyarr notices things, not how it works. A node that
			// cannot enqueue a poll still serves, still plays and still
			// ingests what it already has, and the next beat will try again.
			log.Warn("could not enqueue a download poll",
				"reason", reason, "error", err)
		}
	}
	enqueue("startup")

	go func() {
		ticker := time.NewTicker(downloadPollInterval)
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
	log.Info("download poll beat started", "interval", downloadPollInterval)
}

// hasDownloadClient reports whether configuration declares one.
//
// It reads the RESOLVED capabilities rather than the raw `type`, because a
// provider's abilities come from its kind's defaults unless configuration
// overrides them (providers.DefaultCapabilities). A `type: fake` entry is an
// indexer, a download client, or both, depending entirely on what its
// `capabilities` list says — so matching on the type string would be wrong in
// both directions.
//
// Validation already ran at config load, where a malformed entry stopped the
// process before it opened a database. An error here is therefore not something
// an operator can act on, and the safe reading of a configuration this cannot
// parse is "no download client" — which costs a beat that would have had
// nothing to poll.
func hasDownloadClient(entries []providers.Entry, log *slog.Logger) bool {
	resolved, err := providers.Validate(entries)
	if err != nil {
		log.Warn("could not read the provider configuration for the download poll beat",
			"error", err)
		return false
	}
	for _, r := range resolved {
		for _, capability := range r.Capabilities {
			if capability == providers.CapabilityDownload {
				return true
			}
		}
	}
	return false
}

// enqueueDownloadPoll queues one pass over every configured download client.
//
// The dedupe key is downloads.PollDedupeKey — the queue's existing mechanism
// (invariant 9), and the key that package already declares as one for the whole
// pass. It is what stops a slow poll piling up behind itself, which is the
// failure mode a naive timer produces and produces precisely when the client is
// already struggling; and it is what makes two controller processes, or one
// across a restart, produce a single pass rather than two that would each read
// the client's queue while the other wrote its conclusions.
func enqueueDownloadPoll(ctx context.Context, queue *jobs.Queue) error {
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      downloads.PollJobType,
		DedupeKey: downloads.PollDedupeKey,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing a download poll: %w", err)
	}
	return nil
}
