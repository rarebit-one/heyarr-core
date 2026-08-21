package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Acquisition state (§64, M3-03).
//
// # The event and the row commit together, always
//
// Invariant 7 says every state transition emits, and ADR-0009 says events are
// first-class. Both are only true if the event cannot exist without its row
// and the row cannot exist without its event — so every write here happens
// inside one transaction that also emits, exactly as ConsumptionSession
// (ADR-0024) does.
//
// This is the property that is expensive to retrofit and cheap to hold now:
// an event log with gaps is not a log, it is a sample.

// ErrNoAcquisition is returned when a want has no acquisition row yet.
var ErrNoAcquisition = errors.New("catalog: no acquisition state for that desired item")

// AcquisitionRecord is the stored state plus its bookkeeping.
type AcquisitionRecord struct {
	DesiredItemID string
	State         acquisition.State
	Detail        string
}

const acquisitionColumns = `desired_item_id, phase, managed, content, placement, detail`

func scanAcquisition(row interface{ Scan(...any) error }) (AcquisitionRecord, error) {
	var rec AcquisitionRecord
	var phase, content, placement string
	var managed int
	if err := row.Scan(&rec.DesiredItemID, &phase, &managed, &content, &placement,
		&rec.Detail); err != nil {
		return AcquisitionRecord{}, err
	}
	rec.State = acquisition.State{
		Phase:     acquisition.Phase(phase),
		Managed:   managed == 1,
		Content:   acquisition.Satisfaction(content),
		Placement: acquisition.Satisfaction(placement),
	}
	return rec, nil
}

// Acquisition reads one want's acquisition state.
func (c *Catalog) Acquisition(ctx context.Context, desiredItemID string) (AcquisitionRecord, error) {
	rec, err := scanAcquisition(c.db.Reader().QueryRowContext(ctx,
		`SELECT `+acquisitionColumns+` FROM acquisition_state WHERE desired_item_id = ?`,
		desiredItemID))
	if errors.Is(err, sql.ErrNoRows) {
		return AcquisitionRecord{}, ErrNoAcquisition
	}
	return rec, err
}

// StartAcquisition creates the resting state for a new want.
//
// Idempotent: a want that already has acquisition state keeps it. The job that
// calls this will be re-run (invariant 9), and re-running it must not reset a
// want that is halfway through a download.
func (c *Catalog) StartAcquisition(ctx context.Context, desiredItemID string) (AcquisitionRecord, error) {
	initial := acquisition.Initial()
	now := c.clock.Now().Format(timestampFormat)

	var (
		rec     AcquisitionRecord
		ev      events.Event
		created bool
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO acquisition_state
				(desired_item_id, phase, managed, content, placement, detail,
				 phase_entered_at, created_at, updated_at)
			VALUES (?, ?, 0, ?, ?, '', ?, ?, ?)
			ON CONFLICT (desired_item_id) DO NOTHING`,
			desiredItemID, string(initial.Phase), string(initial.Content),
			string(initial.Placement), now, now, now)
		if err != nil {
			return fmt.Errorf("catalog: starting acquisition for %s: %w", desiredItemID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Already exists. Not a transition, so no event: emitting here
			// would put one in the log on every reconciliation pass.
			rec, err = acquisitionInTx(ctx, tx, desiredItemID)
			return err
		}
		created = true
		rec = AcquisitionRecord{DesiredItemID: desiredItemID, State: initial}
		ev, err = c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"transition":      "created",
				"from":            "",
				"to":              string(initial.Phase),
				"state":           initial.Name(),
			})
		return err
	})
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if created {
		c.events.Publish(ev)
	}
	return rec, nil
}

func acquisitionInTx(ctx context.Context, tx *sql.Tx, id string) (AcquisitionRecord, error) {
	rec, err := scanAcquisition(tx.QueryRowContext(ctx,
		`SELECT `+acquisitionColumns+` FROM acquisition_state WHERE desired_item_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AcquisitionRecord{}, ErrNoAcquisition
	}
	return rec, err
}

// AdvanceAcquisition applies a pipeline transition (§64).
//
// The read, the transition, the write and the event are all inside one
// transaction. Reading outside it would let two workers both see `selected`
// and both queue the same release — which is the duplicate grab the whole
// idempotency story exists to prevent.
func (c *Catalog) AdvanceAcquisition(
	ctx context.Context, desiredItemID string, t acquisition.Transition, detail string,
) (AcquisitionRecord, error) {
	now := c.clock.Now().Format(timestampFormat)

	var (
		rec AcquisitionRecord
		ev  events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, desiredItemID)
		if err != nil {
			return err
		}
		after, err := before.State.Apply(t)
		if err != nil {
			// An illegal transition is the caller's mistake, not a database
			// failure, and it must reach the API as one.
			return err
		}
		if err := writeAcquisition(ctx, tx, desiredItemID, after, detail, now, true); err != nil {
			return err
		}
		rec = AcquisitionRecord{DesiredItemID: desiredItemID, State: after, Detail: detail}
		// Invariant 7, inside the transaction that wrote the row.
		ev, err = c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"transition":      string(t),
				"from":            string(before.State.Phase),
				"to":              string(after.Phase),
				"state":           after.Name(),
				"was":             before.State.Name(),
				"detail":          detail,
			})
		return err
	})
	if err != nil {
		return AcquisitionRecord{}, err
	}
	c.events.Publish(ev)
	return rec, nil
}

// SetSatisfaction records reconciliation's answers on the two axes (§56).
//
// Separate from AdvanceAcquisition because satisfaction is not a pipeline
// event: it is the result of reconciliation, which runs on its own schedule
// and can change the answer without anything having been acquired — a quality
// profile edit can unsatisfy a want that nothing else touched (§57).
//
// A call that changes nothing emits nothing. Reconciliation runs over the
// whole library on a timer, and emitting per row per pass would turn the event
// log into a heartbeat.
func (c *Catalog) SetSatisfaction(
	ctx context.Context, desiredItemID string, content, placement acquisition.Satisfaction,
) (AcquisitionRecord, error) {
	now := c.clock.Now().Format(timestampFormat)

	var (
		rec     AcquisitionRecord
		ev      events.Event
		changed bool
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, desiredItemID)
		if err != nil {
			return err
		}
		after, err := before.State.WithContent(content)
		if err != nil {
			return err
		}
		if after, err = after.WithPlacement(placement); err != nil {
			return err
		}
		rec = AcquisitionRecord{DesiredItemID: desiredItemID, State: after, Detail: before.Detail}
		if after == before.State {
			return nil
		}
		changed = true
		// phase_entered_at is NOT touched: the pipeline did not move, and
		// resetting it here would hide a want stuck in `downloading` for a
		// week behind a reconciliation pass that ran five minutes ago.
		if err := writeAcquisition(ctx, tx, desiredItemID, after, before.Detail, now, false); err != nil {
			return err
		}
		ev, err = c.events.EmitTx(ctx, tx, events.TypeAcquisitionSatisfaction,
			"desired_item", desiredItemID, map[string]any{
				"desired_item_id": desiredItemID,
				"content":         string(after.Content),
				"placement":       string(after.Placement),
				"state":           after.Name(),
				"was":             before.State.Name(),
			})
		return err
	})
	if err != nil {
		return AcquisitionRecord{}, err
	}
	if changed {
		c.events.Publish(ev)
	}
	return rec, nil
}

func writeAcquisition(
	ctx context.Context, tx *sql.Tx, id string,
	s acquisition.State, detail, now string, phaseMoved bool,
) error {
	managed := 0
	if s.Managed {
		managed = 1
	}
	stmt := `UPDATE acquisition_state
	            SET phase = ?, managed = ?, content = ?, placement = ?, detail = ?,
	                updated_at = ?
	          WHERE desired_item_id = ?`
	args := []any{
		string(s.Phase), managed, string(s.Content), string(s.Placement),
		detail, now, id,
	}
	if phaseMoved {
		stmt = `UPDATE acquisition_state
		           SET phase = ?, managed = ?, content = ?, placement = ?, detail = ?,
		               updated_at = ?, phase_entered_at = ?
		         WHERE desired_item_id = ?`
		args = []any{
			string(s.Phase), managed, string(s.Content), string(s.Placement),
			detail, now, now, id,
		}
	}
	_, err := tx.ExecContext(ctx, stmt, args...)
	return err
}
