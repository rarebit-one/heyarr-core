package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// The upgrade scan and supersession (§60, M3-06).
//
// # The edge, again
//
// The decision — is there a strict improvement, and why — is a pure function
// in internal/domain/acquisition. What lives here is the query that finds the
// incumbent and the write that supersedes it.
//
// # Supersession is a logical delete, and the ORDER matters
//
// ADR-0018 governs: the Asset stops existing, the Blob does not, and a later
// gc_blobs sweep reclaims what nothing references once a grace window has
// passed. Unlinking bytes here would make a bug unrecoverable.
//
// The incumbent goes only AFTER the replacement is under management. An
// upgrade that fails once the incumbent is gone is the worst outcome this
// feature can produce — a want that was satisfied is now empty, because
// Heyarr tried to improve it.

// UpgradeCandidateRow is one want the upgrade scan considered.
type UpgradeCandidateRow struct {
	DesiredItemID string
	WorkID        string
	Verdict       acquisition.UpgradeVerdict
	// IncumbentID is the asset currently satisfying the want, when one is.
	IncumbentID string
}

// UpgradeScanResult is what one sweep concluded.
type UpgradeScanResult struct {
	// Considered is every monitored want the scan looked at.
	Considered int
	// Eligible are the wants where nothing about their STATE rules an upgrade
	// out: monitored, satisfied, not yet terminal.
	Eligible []UpgradeCandidateRow
	// Changed reports whether anything was recorded. A scan over a library
	// with nothing upgradable changes nothing and emits nothing.
	Changed bool
}

// ScanForUpgrades finds the wants that could be improved (§60).
//
// It answers the ELIGIBILITY question only — monitored, satisfied, not
// terminal — and deliberately does not search. "Which of my wants might
// improve" is a listing question answerable from state alone and cheap enough
// to run over a whole library; "is this particular release better" needs a
// search first and belongs to the search job (M3-12). Fusing them would make
// this scan perform a provider round trip per row.
//
// Idempotent: it reads and concludes, writing nothing unless a want's
// eligibility actually changed.
func (c *Catalog) ScanForUpgrades(ctx context.Context, limit int) (UpgradeScanResult, error) {
	// Monitored wants only, at the QUERY. Filtering in Go would mean reading
	// every want in the library to discard most of them, and the index on
	// desired_items(monitor) exists precisely so this does not.
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT d.id, d.work_id, d.quality_profile_id, a.content, a.managed
		FROM desired_items d
		JOIN acquisition_state a ON a.desired_item_id = d.id
		WHERE d.monitor = 1
		ORDER BY d.created_at, d.id
		LIMIT ?`, limit)
	if err != nil {
		return UpgradeScanResult{}, fmt.Errorf("catalog: listing monitored wants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		id, workID, profileID, content string
		managed                        bool
	}
	var candidates []row
	for rows.Next() {
		var r row
		var managed int
		if err := rows.Scan(&r.id, &r.workID, &r.profileID, &r.content, &managed); err != nil {
			return UpgradeScanResult{}, err
		}
		r.managed = managed == 1
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return UpgradeScanResult{}, err
	}

	result := UpgradeScanResult{Considered: len(candidates)}
	for _, r := range candidates {
		if err := ctx.Err(); err != nil {
			return result, nil
		}

		satisfied := r.content == string(acquisition.SatisfactionSatisfied)

		// The incumbent's evaluation, recomputed from the same assets and the
		// same profile reconciliation used. Not cached: a profile edit
		// changes the answer, and a cached score would report an upgrade
		// against a standard nobody is using any more.
		incumbent, incumbentID, err := c.incumbentEvaluation(ctx, r.id)
		if err != nil {
			return result, err
		}

		verdict := acquisition.UpgradableVerdict(true /* monitored by the query */, satisfied, incumbent)
		if !verdict.Status.Upgradable() && verdict.Status != acquisition.UpgradeNoBetterCandidate {
			// Ineligible: terminal, or not satisfied. Not reported as a
			// candidate, and not an error — most wants are in one of these
			// states most of the time.
			continue
		}
		result.Eligible = append(result.Eligible, UpgradeCandidateRow{
			DesiredItemID: r.id,
			WorkID:        r.workID,
			Verdict:       verdict,
			IncumbentID:   incumbentID,
		})
	}
	return result, nil
}

// incumbentEvaluation re-scores what a want currently holds.
//
// It reuses assetsForWant and EvaluateContent — the same query and the same
// scorer reconciliation uses — so "is this good enough" has one answer. A
// second query here that selected assets differently would let a want be
// satisfied by reconciliation and unsatisfied by the upgrade scan, which is
// the sort of disagreement nobody would ever debug.
func (c *Catalog) incumbentEvaluation(
	ctx context.Context, desiredItemID string,
) (acquisition.Evaluation, string, error) {
	want, err := c.desiredForReconcile(ctx, desiredItemID)
	if err != nil {
		return acquisition.Evaluation{}, "", err
	}
	profile, err := c.profileForReconcile(ctx, want.qualityProfileID)
	if err != nil {
		return acquisition.Evaluation{}, "", err
	}
	assets, err := c.assetsForWant(ctx, want)
	if err != nil {
		return acquisition.Evaluation{}, "", err
	}

	verdict := acquisition.EvaluateContent(assets, profile)
	if verdict.SatisfiedBy == "" {
		return acquisition.Evaluation{}, "", nil
	}
	for _, e := range verdict.Evaluations {
		if e.AssetID == verdict.SatisfiedBy {
			return e.Evaluation, e.AssetID, nil
		}
	}
	return acquisition.Evaluation{}, "", nil
}

// ErrIncumbentNotSuperseded is returned when supersession is asked for before
// the replacement is under management.
//
// A typed error rather than a message because the caller has to be able to
// tell "you asked too early" from "the database broke", and because getting
// this wrong is the failure mode that leaves a satisfied want empty.
var ErrIncumbentNotSuperseded = errors.New(
	"catalog: the replacement is not under management yet, so the incumbent stays")

// SupersedeIncumbent replaces one asset with another, logically (ADR-0018).
//
// # The order is the whole safety property
//
// The replacement must already exist as a managed asset. If it does not, this
// refuses and the incumbent stays: an upgrade that fails after the incumbent
// is gone turns a satisfied want into an empty one, which is worse than never
// having upgraded at all.
//
// The bytes are NOT unlinked. The Asset row goes, the Blob stays, and gc_blobs
// reclaims it later if nothing else references it — which, for a re-encode of
// the same film, is exactly the grace window an operator wants when they
// decide the new copy was worse.
func (c *Catalog) SupersedeIncumbent(
	ctx context.Context, desiredItemID, incumbentID, replacementID string,
) error {
	if incumbentID == "" || replacementID == "" {
		return fmt.Errorf("catalog: supersession needs both an incumbent and a replacement")
	}
	if incumbentID == replacementID {
		// Nothing to do, and almost certainly a caller bug. Refusing is
		// better than deleting the asset that was supposed to survive.
		return fmt.Errorf("catalog: %s cannot supersede itself", incumbentID)
	}

	now := c.clock.Now().Format(timestampFormat)
	var ev events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		// The replacement must be under management BEFORE the incumbent goes.
		var replacementClass string
		var replacementBlob sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT source_class, blob_hash FROM assets WHERE id = ? AND missing_since IS NULL`,
			replacementID).Scan(&replacementClass, &replacementBlob)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrIncumbentNotSuperseded
		}
		if err != nil {
			return err
		}
		// A linked asset has no blob (ADR-0020) but is still a usable local
		// representation, so it may supersede. A managed one with no blob
		// cannot exist — the schema refuses it — so there is nothing further
		// to check here.

		var incumbentBlob sql.NullString
		var incumbentClass string
		err = tx.QueryRowContext(ctx,
			`SELECT source_class, blob_hash FROM assets WHERE id = ?`, incumbentID).
			Scan(&incumbentClass, &incumbentBlob)
		if errors.Is(err, sql.ErrNoRows) {
			// Already gone. Not an error: supersession is idempotent, and the
			// job that calls it will be re-run (invariant 9).
			return nil
		}
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, incumbentID); err != nil {
			return err
		}

		// Invariant 7, inside the transaction that wrote the row. The payload
		// says bytes_removed explicitly because it is the whole point of
		// ADR-0018 and the first question anyone reading the log will have.
		ev, err = c.events.EmitTx(ctx, tx, events.TypeUpgradeSuperseded,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id":  desiredItemID,
				"superseded_asset": incumbentID,
				"replacement":      replacementID,
				"blob_hash":        nullString(incumbentBlob.String),
				"bytes_removed":    false,
				"at":               now,
			})
		return err
	})
	if err != nil {
		return err
	}
	if ev.ID != "" {
		c.events.Publish(ev)
	}
	return nil
}

// RecordUpgradeFound emits that a want has a strictly better release on offer
// (§60, invariant 7).
//
// Separate from the scan so the scan stays a pure read: a sweep that emitted
// as it walked would emit the same thing on every pass, and a beat that
// re-announces the same available upgrade every five minutes is a heartbeat
// rather than an event stream.
func (c *Catalog) RecordUpgradeFound(
	ctx context.Context, desiredItemID string, v acquisition.UpgradeVerdict,
) error {
	if !v.Status.Upgradable() {
		return fmt.Errorf("catalog: %s is not an upgrade", v.Status)
	}
	ev, err := c.events.Emit(ctx, events.TypeUpgradeFound, "desired_item", desiredItemID,
		map[string]any{
			"desired_item_id": desiredItemID,
			"candidate_id":    v.Candidate.ID,
			"candidate_title": v.Candidate.Title,
			"provider":        v.Candidate.Provider,
			"score":           v.Evaluation.Score,
			"improvement":     v.Improvement,
			"detail":          v.Detail,
		})
	if err != nil {
		return err
	}
	// Emit publishes; a second Publish here would deliver the event twice.
	_ = ev
	return nil
}
