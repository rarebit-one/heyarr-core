package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// GrabStore is the part of the catalog the grab handler uses. A
// *catalog.Catalog satisfies it; the handler depends on no more than it calls.
type GrabStore interface {
	Acquisition(ctx context.Context, desiredItemID string) (catalog.AcquisitionRecord, error)
	AdvanceAcquisition(ctx context.Context, desiredItemID string, t acquisition.Transition, detail string) (catalog.AcquisitionRecord, error)
	AdvanceAcquisitionIfSelected(ctx context.Context, desiredItemID, candidateID string, t acquisition.Transition, detail string) (catalog.AcquisitionRecord, error)
	RecordAcquisition(ctx context.Context, a catalog.Acquisition) (bool, error)
	SelectedSource(ctx context.Context, desiredItemID string) (candidateID string, source secret.Value, err error)
}

// GrabReleaseHandler hands a want's selected release to a download client —
// §64's SELECTED → QUEUED edge (#225).
//
// # What was missing
//
// Everything either side of this existed. The search beat drives
// MISSING → SEARCHING → CANDIDATES_FOUND → SELECTED, poll_downloads observes
// transfers and drives QUEUED → DOWNLOADING → VERIFYING, and
// ingest_acquisition brings the bytes under management. Between SELECTED and
// QUEUED there was no job type at all, so a want that had decided what it
// wanted could never act on the decision, and rested in SELECTED looking
// exactly like a transfer in flight.
//
// # It records the acquisition row, and that is not incidental
//
// poll_downloads finds a transfer's want with AcquisitionByExternal, and
// deliberately refuses to adopt a labelled transfer it has no row for — that
// path exists so one Heyarr cannot attach another's work to a want of its own.
// A grab that started a transfer without writing the row would therefore hand
// the poll a transfer it is required to ignore, and the want would advance no
// further than it does today. The row is what connects the two halves.
//
// # Idempotent at three layers, because it will be re-run (invariant 9)
//
// The queue dedupes on the want (GrabDedupeKey). The state machine refuses
// TransitionQueue from any phase but SELECTED, so a second run cannot
// double-advance. And the clients themselves converge: Transmission answers a
// duplicate add with the existing transfer, which downloads.Client.Add returns
// rather than treating as an error. The last of those is the one that matters
// on a lease that expired mid-call, where the transfer was created and the row
// was not.
func GrabReleaseHandler(
	reg *providers.Registry, cat GrabStore, log *slog.Logger, opts ...GrabOption,
) HandlerFunc {
	var o grabOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, job jobs.Job) error {
		payload, err := decodePayload[acquisition.GrabPayload](job)
		if err != nil {
			return err
		}
		if payload.DesiredItemID == "" {
			return errors.New("worker: grab_release needs a desired item")
		}

		state, err := cat.Acquisition(ctx, payload.DesiredItemID)
		if err != nil {
			return err
		}
		// Not in a phase that can be queued. Expected rather than exceptional:
		// a re-run after the first grab succeeded lands here, and so does a
		// want whose transfer is already downloading.
		//
		// nil rather than an error, for the same reason the search handler
		// returns nil when something is in flight — the job did the right
		// thing, and failing it would retry the same refusal on a backoff.
		if _, err := acquisition.Advance(state.State.Phase, acquisition.TransitionQueue); err != nil {
			log.Info("skipping a grab: the want is not waiting to be queued",
				"desired_item_id", payload.DesiredItemID, "phase", string(state.State.Phase))
			return nil
		}

		candidateID, source, err := cat.SelectedSource(ctx, payload.DesiredItemID)
		if errors.Is(err, catalog.ErrNoSelection) {
			// The selection went away between enqueue and now — a later search
			// superseded it. The grab no longer applies; it is not a failure.
			log.Info("skipping a grab: the want no longer has a selected release",
				"desired_item_id", payload.DesiredItemID)
			return nil
		}
		if err != nil {
			return err
		}
		if payload.CandidateID != "" && payload.CandidateID != candidateID {
			// A different release is selected than the one this job was
			// created for: a later search or a by-hand override replaced it.
			// This job grabs what the want selects NOW.
			//
			// It cannot stand down and leave the work to "the grab queued for
			// the new selection", because there may be no such grab: the queue
			// dedupes on the want against pending AND leased jobs, so a
			// selection that lands while this job is pending or running
			// collapses its enqueue into this very job (#793). This job is
			// the want's only grab, and its payload's candidate is a record of
			// why it was queued, not a constraint on what it fetches.
			log.Info("the selection changed after the grab was queued; grabbing the current one",
				"desired_item_id", payload.DesiredItemID,
				"queued_for", payload.CandidateID, "now", candidateID)
		}

		transfer, provider, err := reg.Grab(ctx, source)
		if errors.Is(err, providers.ErrNoSource) {
			// The indexer offered no way to fetch this release. Distinct from
			// every other failure here: retrying cannot help, because nothing
			// about this candidate will change. The want is failed so it
			// leaves SELECTED — where it would otherwise rest forever, looking
			// like a transfer in flight, which is the exact shape of #225.
			//
			// Failing it also blocks the release for this want, so the next
			// search does not select the same unfetchable candidate again.
			detail := fmt.Sprintf("the indexer offers no way to fetch %s", candidateID)
			if _, aerr := cat.AdvanceAcquisition(ctx, payload.DesiredItemID,
				acquisition.TransitionFail, detail); aerr != nil {
				return aerr
			}
			log.Warn("a selected release cannot be fetched",
				"desired_item_id", payload.DesiredItemID, "candidate", candidateID)
			return nil
		}
		if err != nil {
			// Everything else — no client configured, every client refused,
			// the client is down — is worth retrying. The want stays SELECTED
			// and the job backs off.
			//
			// The error is returned rather than swallowed so the job's
			// last_error carries it: "why is nothing downloading" has to be
			// answerable from the queue, and #225's whole lesson is that a
			// want resting quietly is how this goes unnoticed.
			return fmt.Errorf("worker: grabbing %s for %s: %w",
				candidateID, payload.DesiredItemID, err)
		}

		// The row FIRST, then the transition.
		//
		// This order is deliberate. If the process dies between them, the row
		// exists and the want is still SELECTED: the re-run finds the transfer
		// already present at the client, gets it back from the idempotent Add,
		// rewrites the same row and advances. The other order would leave a
		// want QUEUED with no row, which poll_downloads is required to ignore —
		// a permanently stranded want, from a crash in a one-line window.
		row := catalog.TransferToAcquisition(
			NewAcquisitionID(), payload.DesiredItemID, provider, transfer, "")
		if _, err := cat.RecordAcquisition(ctx, row); err != nil {
			return err
		}

		// Publish the choice for siblings BEFORE the transition (ADR-0119).
		// A failure here returns an error so the job retries: the want is
		// still SELECTED, the client's Add is idempotent, the row is rewritten,
		// and the op gets another chance. After the transition a retry would
		// skip at the top ("not waiting to be queued") and the op would never
		// be written.
		if o.publisher != nil {
			err := o.publisher.Publish(ctx, payload.DesiredItemID, candidateID, transfer)
			if errors.Is(err, ErrSelectionChanged) {
				// The same race as the check above, one step later: a by-hand
				// override replaced the selection while the client was taking
				// this release. Advancing now would mark the want QUEUED on
				// the OLD transfer and the override's choice would never be
				// grabbed; completing without advancing would strand the want
				// in SELECTED, because the override's enqueue collapsed into
				// this leased job (#793). So the job fails and retries: the
				// want is still SELECTED, and the re-run grabs the current
				// selection. The transfer already at the client keeps its row
				// and is reconciled by the poll like any other.
				return fmt.Errorf("worker: the selection for %s changed while %s was being grabbed; retrying for the current one: %w",
					payload.DesiredItemID, candidateID, err)
			}
			if err != nil {
				return err
			}
		}

		// Guarded by the selection, in the transition's own transaction: an
		// override that commits after the publish read the old selection
		// would otherwise be overwritten by a QUEUED naming the old transfer,
		// and its enqueue collapsed into this job (#793). The retry grabs the
		// current selection and publishes it with its newer selected-at, which
		// supersedes the op just written at every sibling.
		if _, err := cat.AdvanceAcquisitionIfSelected(ctx, payload.DesiredItemID, candidateID,
			acquisition.TransitionQueue,
			fmt.Sprintf("queued %s at %s", candidateID, provider)); err != nil {
			if errors.Is(err, catalog.ErrSelectionChanged) {
				return fmt.Errorf("worker: the selection for %s changed before %s was queued; retrying for the current one: %w",
					payload.DesiredItemID, candidateID, err)
			}
			return err
		}

		// The source is NOT logged, here or anywhere in this handler. It is a
		// secret.Value and would redact, but the habit is the control: the
		// candidate id and the provider are what an operator needs, and both
		// are safe to write down.
		log.Info("a release was handed to a download client",
			"desired_item_id", payload.DesiredItemID,
			"candidate", candidateID, "provider", provider, "transfer", transfer.ID)
		return nil
	}
}

// GrabOption tunes GrabReleaseHandler.
type GrabOption func(*grabOptions)

type grabOptions struct {
	publisher AcquirePublisher
}

// WithAcquirePublisher publishes each grabbed release as an acquire op
// (ADR-0119) so siblings acquire it themselves. Nil publishes nothing.
func WithAcquirePublisher(p AcquirePublisher) GrabOption {
	return func(o *grabOptions) {
		if p != nil {
			o.publisher = p
		}
	}
}

// enqueueGrab queues the grab for a want that has just selected a release.
//
// Shared by the search beat and by the by-hand selection endpoint, because a
// want reaches SELECTED by both routes and a grab wired into only one of them
// would leave the other exactly as stranded as everything was before #225.
//
// A failure to enqueue is logged and swallowed, matching enqueueProbe and
// enqueueIngest: the selection itself is durable and correct, and discarding it
// because a follow-up job could not be queued would throw away the search that
// produced it. The want stays SELECTED and the next pass re-enqueues.
func enqueueGrab(
	ctx context.Context, q ProbeEnqueuer, desiredItemID, candidateID string, log *slog.Logger,
) {
	if q == nil {
		// No queue wired. The by-hand selection path can be constructed
		// without one, and a nil dereference there would turn a working
		// endpoint into a panic for the sake of an optimisation.
		return
	}
	if err := queueGrab(ctx, q, desiredItemID, candidateID); err != nil {
		log.Warn("could not queue the grab for a selected release",
			"desired_item_id", desiredItemID, "candidate", candidateID, "error", err)
	}
}

// queueGrab is enqueueGrab's enqueue, with the error returned for a caller
// that must not record success without it (the fan-out applier).
func queueGrab(ctx context.Context, q ProbeEnqueuer, desiredItemID, candidateID string) error {
	_, err := q.Enqueue(ctx, jobs.EnqueueOptions{
		Type: acquisition.GrabJobType,
		Payload: acquisition.GrabPayload{
			DesiredItemID: desiredItemID,
			CandidateID:   candidateID,
		},
		DedupeKey:          acquisition.GrabDedupeKey(desiredItemID),
		RequiredCapability: providers.CapabilityDownload.JobCapability(),
	})
	return err
}
