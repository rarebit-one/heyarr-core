package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// PollDownloadsHandler asks every download client what it is doing and drives
// §64's pipeline from the answer (§58, M3-10).
//
// # It only ever sees Heyarr's own transfers
//
// Downloader.Transfers filters on the label before this handler sees anything.
// The operator's own torrents are not merely skipped here, they are absent —
// so there is no path through this code that can act on one.
//
// # Idempotent, and silent when nothing changed
//
// It will be re-run (invariant 9). Running it twice over an unchanged queue
// writes the same rows the second time and emits nothing: a poll on a timer
// that emitted per pass would turn the event log into a heartbeat, and an event
// stream that is mostly noise is one nobody follows.
//
// # One bad client does not stop the pass
//
// A download client that is unreachable is logged and skipped, not returned as
// an error. Failing the job would mean one unreachable client stops every other
// one from being polled, and would put the pass into a backoff that reports the
// same thing more slowly.
func PollDownloadsHandler(
	reg *providers.Registry, cat *catalog.Catalog, log *slog.Logger,
) HandlerFunc {
	return func(ctx context.Context, _ jobs.Job) error {
		clients := reg.Downloaders()
		if len(clients) == 0 {
			// No download client configured. A supported deployment
			// (ADR-0025), not an error, and not worth a log line per pass.
			return nil
		}

		var advanced, failed int
		for _, client := range clients {
			if ctx.Err() != nil {
				// The lease is tied to this context. Stopping mid-pass is safe
				// rather than wasteful: each transfer's conclusion is committed
				// as it is reached, and the next pass re-derives the rest.
				return nil
			}

			transfers, err := client.Transfers(ctx)
			if err != nil {
				failed++
				log.Warn("could not read a download client's queue",
					"provider", client.Name(), "error", err)
				continue
			}

			for _, t := range transfers {
				n, err := reconcileTransfer(ctx, cat, client.Name(), t, log)
				if err != nil {
					failed++
					log.Warn("could not reconcile a transfer",
						"provider", client.Name(), "transfer", t.ID, "error", err)
					continue
				}
				advanced += n
			}
		}

		// Logged only when something happened. A pass over a steady queue is
		// the normal case and should be invisible.
		if advanced > 0 || failed > 0 {
			log.Info("polled download clients",
				"clients", len(clients), "advanced", advanced, "failed", failed)
		}
		return nil
	}
}

// reconcileTransfer moves one transfer's want along §64's pipeline.
//
// Returns how many state transitions it caused, so the caller can log a pass
// that did something and stay quiet about one that did not.
func reconcileTransfer(
	ctx context.Context, cat *catalog.Catalog, provider string,
	t providers.Transfer, log *slog.Logger,
) (int, error) {
	existing, err := cat.AcquisitionByExternal(ctx, provider, t.ID)
	switch {
	case errors.Is(err, catalog.ErrNoAcquisitionRow):
		// A transfer carrying Heyarr's label that Heyarr has no row for.
		//
		// This is what a restart looks like from the other side: the daemon
		// kept the transfer, the row was lost, or an operator moved the
		// database. It is NOT an error and it must not be adopted either —
		// adopting it would attach somebody else's Heyarr's work to a want of
		// ours, which is the same class of mistake the label prevents, arriving
		// from a different direction.
		//
		// So it is reported and left alone. The transfer keeps running and an
		// operator can see it in the client.
		log.Info("a labelled transfer has no acquisition row",
			"provider", provider, "transfer", t.ID, "name", t.Name)
		return 0, nil
	case err != nil:
		return 0, err
	}

	// The row is refreshed on every pass regardless of whether the pipeline
	// moves: progress is a fact worth having between transitions, and it is
	// what makes "stuck since Tuesday" visible.
	updated := catalog.TransferToAcquisition(
		existing.ID, existing.DesiredItemID, provider, t, existing.LocalPath)
	if _, err := cat.RecordAcquisition(ctx, updated); err != nil {
		return 0, err
	}

	return advancePipeline(ctx, cat, existing.DesiredItemID, t, log)
}

// advancePipeline applies the §64 transition this transfer's state implies.
//
// Each edge is applied only when the machine is in a phase that admits it —
// the state machine refuses the rest, and a refusal here is expected rather
// than exceptional: a poll pass sees the same completed transfer many times
// and must move it exactly once.
func advancePipeline(
	ctx context.Context, cat *catalog.Catalog, desiredItemID string,
	t providers.Transfer, log *slog.Logger,
) (int, error) {
	state, err := cat.Acquisition(ctx, desiredItemID)
	if err != nil {
		return 0, err
	}

	var want acquisition.Transition
	var detail string
	switch {
	case t.Error != "":
		// Including the invisible tracker stall, which reached us as an Error
		// because stall.go read trackerStats. Without that this branch would
		// never fire for the most common stall there is.
		want, detail = acquisition.TransitionFail, t.Error
	case t.Done:
		// To VERIFYING, never straight to ingest: a download client's claim of
		// completion is a claim by a third party about bytes it fetched from
		// strangers (invariant 1).
		want, detail = acquisition.TransitionDownloaded, ""
	case t.BytesDone > 0:
		want, detail = acquisition.TransitionStartDownload, ""
	default:
		return 0, nil
	}

	if _, err := state.State.Apply(want); err != nil {
		// Illegal from here, which is the normal case on a repeat pass: the
		// transfer is still complete and the want moved on three passes ago.
		// Not an error and not worth logging.
		return 0, nil //nolint:nilerr // an illegal transition here means "already moved"
	}

	if _, err := cat.AdvanceAcquisition(ctx, desiredItemID, want, detail); err != nil {
		return 0, err
	}
	log.Info("a transfer advanced its acquisition",
		"desired_item_id", desiredItemID, "transition", string(want), "detail", detail)
	return 1, nil
}

// NewAcquisitionID mints an identifier for an acquisition row.
//
// Here rather than inline so that the one place a UUIDv7 is minted for this
// table is greppable (ADR-0017).
func NewAcquisitionID() string { return uuid.Must(uuid.NewV7()).String() }

// downloadClientNames lists the configured download clients, for the startup
// log — the same reasoning as indexerNames.
func downloadClientNames(reg *providers.Registry) []string {
	var out []string
	for _, p := range reg.Route(providers.CapabilityDownload) {
		out = append(out, p.Name())
	}
	return out
}
