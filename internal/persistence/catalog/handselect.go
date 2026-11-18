package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// A by-hand decision about a want that already has a grab in flight (#888).
//
// A select or a block used to change the candidate rows and nothing else. With
// a transfer already at the client, the grab the select queued skipped ("not
// waiting to be queued"), the block left the transfer downloading, and the
// caller was told both had worked. These two operations change the pipeline
// too, in the transaction that changes the selection, and hand the caller the
// transfer they took the want off, to remove at its client by a job: the API
// that answers the person may not be the node that has the client (invariant 4).

// ErrSearchInFlight is a select while a search is running: the search is about
// to replace the candidates and its own pick would overwrite the choice.
var ErrSearchInFlight = errors.New("catalog: a search is running for this want")

// ErrTransferInProgress is a select that would replace a transfer that has
// already fetched bytes. The caller may force it.
var ErrTransferInProgress = errors.New("catalog: the want's transfer has already fetched bytes")

// ErrImportInProgress is a select while the want's download is being verified
// or imported: the bytes are already here.
var ErrImportInProgress = errors.New("catalog: the want's download is being verified and imported")

// Displaced is a transfer a by-hand decision took its want off. Its row is
// gone, so the poll no longer moves the want by it, and it is still at its
// client until something removes it.
type Displaced struct {
	DesiredItemID string
	Provider      string
	ExternalID    string
	ExternalName  string
	BytesDone     int64
	BytesTotal    int64
}

// HandSelection is the outcome of SelectByHand.
type HandSelection struct {
	Candidate Candidate
	// Phase is where the want rests afterwards.
	Phase acquisition.Phase
	// Displaced is the transfer the selection replaced, nil when there was
	// none.
	Displaced *Displaced
	// Changed reports that the want now needs a grab for Candidate.
	Changed bool
}

// SelectByHand is §60's manual override, made against whatever the want is
// doing (#888). In one transaction it moves the selection and the pipeline:
//
//	idle, candidates_found  selected, a grab to queue
//	selected                the pending grab takes the new selection
//	searching               ErrSearchInFlight: the search is about to replace the candidates
//	queued, downloading     the transfer is replaced: cancel to idle, select,
//	                        its row dropped and the transfer handed back as Displaced
//	verifying, ingesting    ErrImportInProgress: the bytes are here, select once they are held
//
// A transfer that has fetched any bytes is replaced only with force, and
// ErrTransferInProgress says how far it got otherwise. Zero bytes is the line
// because it is the one the operator can see without arithmetic: a transfer
// that fetched nothing has cost nothing, and anything past it is a judgement
// the operator makes, not this method. Naming the release the transfer is
// already fetching changes nothing.
func (c *Catalog) SelectByHand(
	ctx context.Context, desiredItemID, candidateID string, force bool,
) (HandSelection, error) {
	now := sqlite.FormatTimestamp(c.clock.Now())
	var (
		out     HandSelection
		pending []events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, desiredItemID)
		if err != nil {
			return err
		}
		phase := before.State.Phase
		switch phase {
		case acquisition.PhaseSearching:
			return ErrSearchInFlight
		case acquisition.PhaseVerifying, acquisition.PhaseIngesting:
			return fmt.Errorf("%w (%s)", ErrImportInProgress, before.State.Name())
		}

		// The transfer in flight, and whether it is the selection's.
		var (
			row        *Acquisition
			rowRelease string
		)
		if phase == acquisition.PhaseQueued || phase == acquisition.PhaseDownloading {
			r, grabbedAt, err := acquisitionRowTx(ctx, tx, desiredItemID)
			if err != nil {
				return err
			}
			if r != nil {
				row = r
				_, rel, _, found, err := transferSelection(ctx, tx, desiredItemID, r.ExternalID, grabbedAt)
				if err != nil {
					return err
				}
				if found {
					rowRelease = rel
				}
			}
		}

		chosen, overrideEv, err := c.overrideSelectionTx(ctx, tx, desiredItemID, candidateID)
		if err != nil {
			return err
		}
		if chosen.Overridden {
			pending = append(pending, overrideEv)
		}
		out = HandSelection{Candidate: chosen, Phase: phase}

		var steps []acquisition.Transition
		switch phase {
		case acquisition.PhaseIdle, acquisition.PhaseCandidatesFound:
			steps = []acquisition.Transition{acquisition.TransitionSelect}
		case acquisition.PhaseSelected:
			// The grab already queued fetches what the want selects when it
			// runs, and the caller queues one in case it died.
			out.Changed = true
			return nil
		case acquisition.PhaseQueued, acquisition.PhaseDownloading:
			if row != nil && rowRelease == chosen.CandidateID {
				// The release already in flight. Nothing to replace.
				return nil
			}
			if row != nil && row.BytesDone > 0 && !force {
				return fmt.Errorf("%w: %s is %s, %s fetched",
					ErrTransferInProgress, row.ExternalName, before.State.Name(), progressOf(row.BytesDone, row.BytesTotal))
			}
			steps = []acquisition.Transition{acquisition.TransitionCancel, acquisition.TransitionSelect}
		default:
			return fmt.Errorf("catalog: cannot select by hand while %s", phase)
		}

		after := before.State
		details := map[acquisition.Transition]string{
			acquisition.TransitionCancel: "replaced by hand with " + chosen.CandidateID,
			acquisition.TransitionSelect: "selected by hand",
		}
		for _, t := range steps {
			next, err := after.Apply(t)
			if err != nil {
				return err
			}
			ev, err := emitPhaseChange(ctx, c.events, tx, desiredItemID, t, after, next, details[t])
			if err != nil {
				return err
			}
			pending = append(pending, ev)
			after = next
		}
		if err := writeAcquisition(ctx, tx, desiredItemID, after, details[acquisition.TransitionSelect], now, true); err != nil {
			return err
		}
		if row != nil {
			d, err := dropAcquisitionRowTx(ctx, tx, *row)
			if err != nil {
				return err
			}
			out.Displaced = d
		}
		out.Phase = after.Phase
		out.Changed = true
		return nil
	})
	if err != nil {
		return HandSelection{}, err
	}
	c.events.Publish(pending...)
	return out, nil
}

// CancelInFlightRelease takes a want off the release it is fetching, because
// that release was just blocked (#888). The want returns to idle so a search
// can choose another; the caller clears its backoff and queues that search.
//
// It acts only when the want's selection IS that release and the want is in
// flight on it: SELECTED with its grab still to run, or QUEUED/DOWNLOADING on a
// transfer grabbed for that selection. A block on anything else, or on a want
// already verifying its download, changes only what the next search may
// choose, and cancelled is false. The transfer, when there was one, is handed
// back as Displaced for the caller to remove at its client.
func (c *Catalog) CancelInFlightRelease(
	ctx context.Context, desiredItemID, provider, candidateID, detail string,
) (displaced *Displaced, cancelled bool, err error) {
	now := sqlite.FormatTimestamp(c.clock.Now())
	var pending []events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, desiredItemID)
		if errors.Is(err, ErrNoAcquisition) {
			// Nothing has ever been in flight for it.
			return nil
		}
		if err != nil {
			return err
		}
		phase := before.State.Phase
		if phase != acquisition.PhaseSelected && phase != acquisition.PhaseQueued && phase != acquisition.PhaseDownloading {
			return nil
		}
		var selProvider, selCandidate string
		switch err := tx.QueryRowContext(ctx, `
			SELECT provider, candidate_id FROM release_candidates
			 WHERE desired_item_id = ? AND selected = 1`, desiredItemID,
		).Scan(&selProvider, &selCandidate); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if selProvider != provider || selCandidate != candidateID {
			return nil
		}

		var row *Acquisition
		if phase != acquisition.PhaseSelected {
			r, grabbedAt, err := acquisitionRowTx(ctx, tx, desiredItemID)
			if err != nil {
				return err
			}
			if r != nil {
				_, rel, _, found, err := transferSelection(ctx, tx, desiredItemID, r.ExternalID, grabbedAt)
				if err != nil {
					return err
				}
				if !found || rel != candidateID {
					// In flight on an older release than the one selected:
					// the block does not name what is downloading.
					return nil
				}
				row = r
			}
		}

		after, err := before.State.Apply(acquisition.TransitionCancel)
		if err != nil {
			return err
		}
		if detail == "" {
			detail = "its release was blocked by hand"
		}
		ev, err := emitPhaseChange(ctx, c.events, tx, desiredItemID, acquisition.TransitionCancel, before.State, after, detail)
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		if err := writeAcquisition(ctx, tx, desiredItemID, after, detail, now, true); err != nil {
			return err
		}
		if row != nil {
			if displaced, err = dropAcquisitionRowTx(ctx, tx, *row); err != nil {
				return err
			}
		}
		cancelled = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	c.events.Publish(pending...)
	return displaced, cancelled, nil
}

// acquisitionRowTx reads a want's transfer row and the moment it was grabbed,
// nil when it has none.
func acquisitionRowTx(ctx context.Context, tx *sql.Tx, desiredItemID string) (*Acquisition, string, error) {
	var grabbedAt string
	var a Acquisition
	err := tx.QueryRowContext(ctx, `
		SELECT `+acquisitionCols+`, created_at FROM acquisitions WHERE desired_item_id = ?`,
		desiredItemID).Scan(&a.ID, &a.DesiredItemID, &a.Provider, &a.ExternalID,
		&a.ExternalName, &a.RemotePath, &a.LocalPath,
		&a.BytesTotal, &a.BytesDone, &a.Trouble, &grabbedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("catalog: reading the transfer for %s: %w", desiredItemID, err)
	}
	return &a, grabbedAt, nil
}

// dropAcquisitionRowTx deletes a transfer row and describes what it named.
func dropAcquisitionRowTx(ctx context.Context, tx *sql.Tx, row Acquisition) (*Displaced, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM acquisitions WHERE desired_item_id = ? AND provider = ? AND external_id = ?`,
		row.DesiredItemID, row.Provider, row.ExternalID); err != nil {
		return nil, fmt.Errorf("catalog: dropping the transfer for %s: %w", row.DesiredItemID, err)
	}
	return &Displaced{
		DesiredItemID: row.DesiredItemID, Provider: row.Provider, ExternalID: row.ExternalID,
		ExternalName: row.ExternalName, BytesDone: row.BytesDone, BytesTotal: row.BytesTotal,
	}, nil
}

// emitPhaseChange is the acquisition.phase_changed event advanceAcquisition
// emits, for a method that walks more than one edge in a transaction.
func emitPhaseChange(
	ctx context.Context, log *events.Log, tx *sql.Tx, desiredItemID string,
	t acquisition.Transition, from, to acquisition.State, detail string,
) (events.Event, error) {
	return log.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged,
		"desired_item", desiredItemID, map[string]any{
			"desired_item_id": desiredItemID,
			"transition":      string(t),
			"from":            string(from.Phase),
			"to":              string(to.Phase),
			"state":           to.Name(),
			"was":             from.Name(),
			"detail":          detail,
		})
}

// progressOf renders how far a transfer got, for a refusal an operator reads.
func progressOf(done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("%d bytes", done)
	}
	return fmt.Sprintf("%d of %d bytes (%d%%)", done, total, done*100/total)
}
