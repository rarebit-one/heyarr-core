package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Creating a want, in one place (§55, M3-02, M12).
//
// # Why this is a catalog method and not just the API's
//
// There are two callers that must create a DesiredItem identically: the API's
// WantContent (POST /desired and MCP's want_content), and M12's poll_source
// worker, which projects one item-scoped want per episode a followed source
// emits. If each wrote the row itself the two would drift, and the drift would
// be silent — one would emit the acquisition event and the other would not, or
// one would forget the resting acquisition row and leave a want the
// reconciliation sweep can never advance. That is not hypothetical: desired.go's
// own history records the API writing an acquisition row directly while the
// catalog's StartAcquisition emitted, and an acceptance assertion, not review,
// caught the divergence. One implementation, two callers, is the answer — and
// the worker cannot import the API, so the one implementation lives here.
//
// # It creates the want AND its resting acquisition state, together
//
// A want with no acquisition row is a want the reconciliation sweep cannot
// advance and nothing would notice — it would sit wanted and never searched for,
// the quietest failure this feature has. So the row, the resting state and both
// events commit in one transaction (invariant 7).
//
// # The conflict is surfaced, not swallowed
//
// The insert is plain, so a second want over the same (target, profile) returns
// the unique-violation error rather than being silently ignored (§61 — two
// wants over one target with DIFFERENT profiles are legal and the point; the
// same target AND profile is one want written twice). The API maps it to a 409;
// the worker treats it as "already projected" and moves on, which is what makes
// re-running a poll idempotent (invariant 9). IsDuplicateWant tells the two
// apart from any other failure.

// CreateDesiredItem inserts a validated, fully-resolved want, its resting
// acquisition state, and the two events both imply, in one transaction. The
// item's ids (work, edition, item, quality profile) must already be resolved —
// resolving a work descriptor or a profile name is the API's business, not this
// method's. Returns the created want with its initial acquisition view.
func (c *Catalog) CreateDesiredItem(ctx context.Context, item desired.Item) (DesiredItemRecord, error) {
	if err := item.Validate(); err != nil {
		return DesiredItemRecord{}, err
	}
	if item.ID == "" {
		item.ID = uuid.Must(uuid.NewV7()).String()
	}
	now := c.clock.Now().UTC()
	initial := acquisition.Initial()

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := insertDesiredItem(ctx, tx, item, now); err != nil {
			return err
		}
		if err := insertRestingAcquisition(ctx, tx, item.ID, initial, now); err != nil {
			return err
		}
		// The acquisition event first, then the desired.created event — the same
		// order and the same reason as the API: a subscriber building a pipeline
		// view from acquisition.* alone must see the acquisition appear, or its
		// table is missing rows forever with nothing to say why.
		acqEvent, err := c.events.EmitTx(ctx, tx, events.TypeAcquisitionPhaseChanged,
			"desired_item", item.ID, map[string]any{
				"desired_item_id": item.ID,
				"transition":      "created",
				"from":            "",
				"to":              string(initial.Phase),
				"state":           initial.Name(),
			})
		if err != nil {
			return err
		}
		pending = append(pending, acqEvent)

		kind, target := item.Target()
		createdEvent, err := c.events.EmitTx(ctx, tx, events.TypeDesiredCreated,
			"desired_item", item.ID, map[string]any{
				"desired_item_id":    item.ID,
				"scope":              string(item.Scope),
				"target_type":        kind,
				"target_id":          target,
				"quality_profile_id": item.QualityProfileID,
				"monitor":            item.Monitor,
			})
		if err != nil {
			return err
		}
		pending = append(pending, createdEvent)
		return nil
	})
	if err != nil {
		return DesiredItemRecord{}, err
	}
	c.events.Publish(pending...)

	return DesiredItemRecord{
		Item:      item,
		CreatedAt: now,
		UpdatedAt: now,
		State:     initial,
	}, nil
}

// DesiredItemRecord is a freshly-created want and its resting acquisition state,
// enough for a caller to render the response without reading the row back.
type DesiredItemRecord struct {
	Item      desired.Item
	CreatedAt time.Time
	UpdatedAt time.Time
	State     acquisition.State
}

// IsDuplicateWant reports whether an error from CreateDesiredItem is the
// (target, profile) uniqueness violation rather than any other failure — so the
// worker can treat "already projected" as success while the API maps it to a
// 409.
//
// It matches on the driver's message the same way the API's isUniqueViolation
// does; the pure-Go SQLite driver has no typed constraint error to inspect, so
// the string is the only signal there is.
func IsDuplicateWant(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique")
}

func insertDesiredItem(ctx context.Context, tx *sql.Tx, item desired.Item, now time.Time) error {
	var edition, itemID any
	if item.EditionID != "" {
		edition = item.EditionID
	}
	if item.ItemID != "" {
		itemID = item.ItemID
	}
	monitor := 0
	if item.Monitor {
		monitor = 1
	}
	stamp := now.Format(timestampFormat)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO desired_items
			(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor,
			 reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, string(item.Scope), item.WorkID, edition, itemID, item.QualityProfileID,
		monitor, item.Reason, stamp, stamp)
	if err != nil {
		return fmt.Errorf("catalog: inserting a desired item: %w", err)
	}
	return nil
}

func insertRestingAcquisition(
	ctx context.Context, tx *sql.Tx, desiredItemID string, s acquisition.State, now time.Time,
) error {
	stamp := now.Format(timestampFormat)
	managed := 0
	if s.Managed {
		managed = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO acquisition_state
			(desired_item_id, phase, managed, content, placement, detail,
			 phase_entered_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`,
		desiredItemID, string(s.Phase), managed, string(s.Content), string(s.Placement),
		stamp, stamp, stamp)
	if err != nil {
		return fmt.Errorf("catalog: inserting resting acquisition state: %w", err)
	}
	return nil
}
