package worker

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// ProviderHealthHandler exercises every configured provider and records what it
// found (§59, ADR-0025).
//
// # Why this is a job and not a ticker inside the registry
//
// Invariant 4: roles communicate only through the job table and HTTP, even
// inside `heyarr all`. A health answer that lived only in the checking
// process's memory would be invisible to the controller answering
// GET /api/v1/providers whenever the worker is on another machine — which is a
// supported deployment, not an edge case.
//
// So the pass writes its findings to the database, and the API reads them from
// there. The registry's in-memory copy is a cache for the process that did the
// checking, not the source of truth.
//
// # It exercises rather than asserts
//
// A provider that reported itself healthy because it was configured would be
// advertising something it might not deliver, and work would route to it and
// then fail. Each provider's Check does real I/O and reports what happened.
//
// # An unhealthy provider is not a failed job
//
// The point of the pass is to RECORD that a provider is down. Returning an
// error would mean one unreachable indexer stops the download client from being
// checked at all, and would put the job into a retry backoff that reports the
// same thing more slowly.
//
// Idempotent by construction (invariant 9): it reads the world and writes what
// it saw. Running it twice writes the same answers the second time.
func ProviderHealthHandler(
	reg *providers.Registry, recorder *catalog.Catalog, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, _ jobs.Job) error {
		statuses := reg.CheckAll(ctx)
		if len(statuses) == 0 {
			// No providers configured. Not an error, and not worth a log line
			// on every pass — a node with none is a supported configuration
			// (ADR-0025) and saying so repeatedly is noise.
			return nil
		}

		// Persisting is what makes the answer readable from another process.
		// A failure here IS a job failure: the check ran, and losing what it
		// found is the one outcome that makes the pass pointless.
		if err := recorder.RecordProviderHealth(ctx, statuses); err != nil {
			return err
		}

		var unhealthy int
		for _, s := range statuses {
			if !s.Health.Healthy {
				unhealthy++
				log.Warn("a provider is not healthy",
					"provider", s.Name,
					"capabilities", providers.JoinCapabilities(s.Capabilities),
					"detail", s.Health.Detail,
					"version", s.Health.Version)
			}
		}
		// Logged only when something is wrong. A pass over a healthy set is
		// the normal case and should be invisible — the same reasoning as the
		// reconciliation sweep.
		if unhealthy > 0 {
			log.Info("provider health checked",
				"providers", len(statuses), "unhealthy", unhealthy)
		}
		return nil
	}
}

// indexerNames lists the configured indexers, for the startup log.
//
// A worker that will claim search jobs should say which providers it would use,
// for the same reason it logs which job types it claims: "why is nothing
// searching" should be answerable from the log a worker prints when it starts.
func indexerNames(reg *providers.Registry) []string {
	var out []string
	for _, p := range reg.Route(providers.CapabilityIndexer) {
		out = append(out, p.Name())
	}
	return out
}

// feedProviderNames is the same, for the metadata providers a worker will poll
// followed sources through — so the startup log says which feed adapters it
// would use, and "why is nothing being followed" is answerable from it (M12).
func feedProviderNames(reg *providers.Registry) []string {
	var out []string
	for _, p := range reg.Route(providers.CapabilityMetadata) {
		out = append(out, p.Name())
	}
	return out
}
