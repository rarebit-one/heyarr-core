package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// ErrRepairHold prevents a queued automatic search from clearing a repair hold.
var ErrRepairHold = errors.New("catalog: acquisition is held for local repair")

// ErrNoRepairTransfer tells an explicit retry to search again instead.
var ErrNoRepairTransfer = errors.New("catalog: held acquisition has no transfer to retry")

// HoldAcquisitionForRepair moves an in-flight acquisition to idle and records a
// durable node-local hold in the same transaction as its event. No candidate is
// blamed. A repeated failure is idempotent.
func (c *Catalog) HoldAcquisitionForRepair(ctx context.Context, id, detail string) error {
	if strings.TrimSpace(detail) == "" {
		return errors.New("catalog: repair hold needs a reason")
	}
	var pending []events.Event
	now := sqlite.FormatTimestamp(c.clock.Now())
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		before, err := acquisitionInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		after := before.State
		if after.Phase != acquisition.PhaseIdle {
			after, err = after.Apply(acquisition.TransitionFail)
			if err != nil {
				return err
			}
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT detail FROM acquisition_repair_holds WHERE desired_item_id=?`, id).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && existing == detail && before.State == after && before.Detail == detail {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO acquisition_repair_holds(desired_item_id,detail,created_at) VALUES(?,?,?) ON CONFLICT(desired_item_id) DO UPDATE SET detail=excluded.detail`, id, detail, now)
		if err != nil {
			return err
		}
		if err = writeAcquisition(ctx, tx, id, after, detail, now, before.State.Phase != after.Phase); err != nil {
			return err
		}
		e, err := c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged, "desired_item", id, map[string]any{"desired_item_id": id, "transition": "local_repair_hold", "from": string(before.State.Phase), "to": string(after.Phase), "state": after.Name(), "detail": detail})
		if err != nil {
			return err
		}
		pending = append(pending, e)
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: holding acquisition for repair: %w", err)
	}
	c.events.Publish(pending...)
	return nil
}

// AcquisitionRepairHeld reports whether a want is paused for node-local repair.
func (c *Catalog) AcquisitionRepairHeld(ctx context.Context, id string) (bool, error) {
	var n int
	err := c.db.Reader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acquisition_repair_holds WHERE desired_item_id=?)`, id).Scan(&n)
	return n != 0, err
}

// ClearAcquisitionRepairHold explicitly releases a node-local hold. It does not
// claim the underlying fault is fixed; the ensuing search checks that anew.
func (c *Catalog) ClearAcquisitionRepairHold(ctx context.Context, id string) error {
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM acquisition_repair_holds WHERE desired_item_id=?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		e, err := c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged, "desired_item", id, map[string]any{"desired_item_id": id, "transition": "local_repair_retry"})
		if err != nil {
			return err
		}
		pending = append(pending, e)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// RetryHeldIngest is an explicit retry of the already-completed transfer. It
// clears a repair hold and moves the want back to verifying atomically.
func (c *Catalog) RetryHeldIngest(ctx context.Context, id string) (bool, error) {
	var pending []events.Event
	changed := false
	now := sqlite.FormatTimestamp(c.clock.Now())
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acquisition_repair_holds WHERE desired_item_id=?)`, id).Scan(&held); err != nil {
			return err
		}
		if held == 0 {
			return nil
		}
		var transfer int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acquisitions WHERE desired_item_id=?)`, id).Scan(&transfer); err != nil {
			return err
		}
		if transfer == 0 {
			return ErrNoRepairTransfer
		}
		before, err := acquisitionInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		after, err := before.State.Apply(acquisition.TransitionAdopt)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM acquisition_repair_holds WHERE desired_item_id=?`, id); err != nil {
			return err
		}
		if err = writeAcquisition(ctx, tx, id, after, "retrying after local repair", now, true); err != nil {
			return err
		}
		e, err := c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged, "desired_item", id, map[string]any{"desired_item_id": id, "transition": "local_repair_reingest", "from": string(before.State.Phase), "to": string(after.Phase), "state": after.Name()})
		if err != nil {
			return err
		}
		pending = append(pending, e)
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	c.events.Publish(pending...)
	return changed, nil
}

// requireNoRepairHoldTx must be called inside the mutation's transaction so a
// stale poll or search cannot race a newly committed local-repair hold.
func requireNoRepairHoldTx(ctx context.Context, tx *sql.Tx, id string) error {
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acquisition_repair_holds WHERE desired_item_id=?)`, id).Scan(&held); err != nil {
		return err
	}
	if held != 0 {
		return ErrRepairHold
	}
	return nil
}
