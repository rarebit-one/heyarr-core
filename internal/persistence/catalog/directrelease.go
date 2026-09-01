package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// The direct release — a non-search source's release, recorded whole (§55, §64,
// M12 Phase 2).
//
// # Why a followed podcast needs this and a followed TV series does not
//
// A TV episode has no bytes location until an indexer is searched, so its want
// rests in MISSING and the search beat drives it: MISSING → SEARCHING →
// CANDIDATES_FOUND → SELECTED, one release chosen from many. A podcast episode is
// the opposite — the feed already handed heyarr the one release, an <enclosure>
// URL, so there is nothing to search for and no node with a metadata provider is
// required to also have an indexer. The search job is even registered only where
// an indexer is (worker.go), so a podcast-only node would never run it, and the
// want would sit in MISSING forever.
//
// This closes that gap: the poll records the enclosure as the want's single,
// pre-selected release and walks the acquisition to SELECTED, from where the
// ORDINARY grab (§64's SELECTED → QUEUED) hands it to the KindHTTP download
// client. Everything downstream — grab, verify, ingest, replicate — is untouched.
//
// # The three transitions are honest, not fabricated
//
// The walk emits search, candidates_found and select — the same three a real
// search emits — and TransitionAdopt's comment warns against recording "steps
// that did not happen". They DID happen, in the followed-source sense: the poll
// IS the search (it asked the feed what it has), it found exactly one candidate
// (the enclosure the feed named), and it selected it. There is no indexer round
// trip, but there was a discovery and a choice, and the event log says so.
//
// # One transaction, and idempotent (invariant 9)
//
// A poll will be re-run, and a re-poll re-presents the same episode. So the whole
// walk commits atomically and only from the resting MISSING state: a want already
// past idle — being acquired, or already held — is left untouched and reports
// selected=false, so a re-poll neither duplicates the candidate nor drags a
// downloading want backwards.

// DirectRelease is the provenance reason a directly-fed release carries in place
// of a quality evaluation.
//
// A quality profile RANKS releases a search returned; a feed's enclosure is the
// single authoritative release, with nothing to rank, and a podcast audio file
// carries none of §62's video attributes — so gating it on a video-shaped profile
// would spuriously reject every episode. The release is recorded accepted by
// PROVENANCE (the feed adapter is the identity authority for its source), and this
// is the one Reason that says so on the stored candidate.
var directReleaseReason = acquisition.Reason{
	Rule:    "release/direct-feed",
	Section: "provenance",
	Result:  acquisition.ResultPass,
	Detail:  "supplied directly by the followed source's feed; no indexer search",
}

// RecordDirectRelease records cand as desiredItemID's single, pre-selected
// release and advances the acquisition to SELECTED, in one transaction. It
// reports whether it selected — false means the want was not resting in MISSING
// and was left untouched (idempotent on a re-poll). The caller enqueues the grab.
func (c *Catalog) RecordDirectRelease(
	ctx context.Context, desiredItemID string, cand acquisition.ReleaseCandidate,
) (bool, error) {
	if cand.ID == "" {
		return false, fmt.Errorf("catalog: a direct release needs a candidate id")
	}
	if cand.Source.IsZero() {
		return false, fmt.Errorf("catalog: a direct release needs a source to fetch")
	}

	now := c.clock.Now()
	stamp := now.Format(timestampFormat)
	searchID := uuid.Must(uuid.NewV7()).String()

	eval := acquisition.Evaluation{
		CandidateID: cand.ID,
		Accepted:    true,
		Reasons:     []acquisition.Reason{directReleaseReason},
	}

	var (
		selected bool
		pending  []events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, desiredItemID)
		if err != nil {
			return err
		}
		// Only from the resting MISSING state. Past idle means something is in
		// flight or already ingested; managed means bytes are held. Either way a
		// re-poll must not re-drive it — that is the idempotency this method owes
		// invariant 9.
		if before.State.Phase != acquisition.PhaseIdle || before.State.Managed {
			return nil
		}

		// The walk, on the in-memory state, so the whole of it is one write.
		searching, err := before.State.Apply(acquisition.TransitionSearch)
		if err != nil {
			return err
		}
		found, err := searching.Apply(acquisition.TransitionCandidatesFound)
		if err != nil {
			return err
		}
		chosen, err := found.Apply(acquisition.TransitionSelect)
		if err != nil {
			return err
		}

		// The candidate, selected. Replace first for the same reason RecordSearch
		// does: a resting MISSING want should have none, but a defensive clear
		// keeps the partial-unique "one selected row" index satisfiable.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM release_candidates WHERE desired_item_id = ?`, desiredItemID); err != nil {
			return fmt.Errorf("catalog: clearing previous candidates: %w", err)
		}
		attrs, err := json.Marshal(cand.Attributes)
		if err != nil {
			return fmt.Errorf("catalog: encoding candidate attributes: %w", err)
		}
		evalJSON, err := json.Marshal(eval)
		if err != nil {
			return fmt.Errorf("catalog: encoding a direct-release evaluation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO release_candidates
				(id, desired_item_id, search_id, provider, candidate_id, title,
				 attributes, evaluation, accepted, score, terminal, selected,
				 overridden, override_detail, searched_at, created_at, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 0, 0, 1, 0, '', ?, ?, ?)`,
			uuid.Must(uuid.NewV7()).String(), desiredItemID, searchID,
			cand.Provider, cand.ID, cand.Title, string(attrs), string(evalJSON),
			stamp, stamp, cand.Source.Reveal()); err != nil {
			return fmt.Errorf("catalog: recording a direct release: %w", err)
		}

		// The state, at SELECTED, and the three transitions that reached it — the
		// event log carries every one (invariant 7), the same three a search emits.
		if err := writeAcquisition(ctx, tx, desiredItemID, chosen, "selected a direct release", stamp, true); err != nil {
			return err
		}
		search, err := c.events.EmitTx(ctx, tx, events.TypeSearchCompleted,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"search_id":       searchID,
				"found":           1,
				"acceptable":      1,
				"selected":        cand.ID,
				"direct":          true,
			})
		if err != nil {
			return err
		}
		pending = append(pending, search)

		for _, step := range []struct {
			t        acquisition.Transition
			from, to acquisition.State
		}{
			{acquisition.TransitionSearch, before.State, searching},
			{acquisition.TransitionCandidatesFound, searching, found},
			{acquisition.TransitionSelect, found, chosen},
		} {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged,
				"desired_item", desiredItemID, map[string]any{
					"desired_item_id": desiredItemID,
					"transition":      string(step.t),
					"from":            string(step.from.Phase),
					"to":              string(step.to.Phase),
					"state":           step.to.Name(),
					"was":             step.from.Name(),
				})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		selected = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if selected {
		c.events.Publish(pending...)
	}
	return selected, nil
}
