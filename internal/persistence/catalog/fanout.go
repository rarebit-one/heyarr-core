package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/strategy"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Fan-out acquisition (ADR-0119): a sibling published the release it chose;
// this node ensures the same work and want exist here, so the ordinary grab
// can fetch the same bytes with this node's own download client.

// FanOutProviderPrefix marks a release recorded from a sibling's acquire op:
// the candidate's provider is this prefix plus the origin's site. The grab
// that fetches it does not publish an op of its own (the origin's already
// reaches every site), so it never echoes back.
const FanOutProviderPrefix = "fan-out:"

// FanOutOutcome is what this node did with one acquire op.
type FanOutOutcome string

const (
	// FanOutApplied means the want exists, the release is selected, the grab is queued.
	FanOutApplied FanOutOutcome = "applied"
	// FanOutOwn means this node signed the op; it is the origin, nothing to apply.
	FanOutOwn FanOutOutcome = "own"
	// FanOutHeld means the want already holds acceptable content or is already
	// acquiring; the op changed nothing.
	FanOutHeld FanOutOutcome = "held"
	// FanOutTombstoned means the work is logically deleted here; the op is
	// ignored while it is. Re-evaluated every pass: a restore makes the op
	// effective again.
	FanOutTombstoned FanOutOutcome = "tombstoned"
	// FanOutScopeUnresolved means the op names an edition or item this node has
	// not materialised. Retried on every pass until it appears.
	FanOutScopeUnresolved FanOutOutcome = "scope_unresolved"
	// FanOutNoSiteSource means this node can resolve no source for the
	// release (a private release, while resolving one through this site's own
	// indexers is not built; or no profile to want it under). Nothing is
	// created: a want would be searched by this site's scorer, which could
	// grab a different release. Retried every pass, so a later means of
	// resolving it acts on the ops already received.
	FanOutNoSiteSource FanOutOutcome = "no_site_source"
	// FanOutProfileUnknown is not an outcome of its own: an unknown profile
	// name falls back to the content type's default and the fallback is
	// noted in the detail. The constant exists so the CHECK list is complete.
	FanOutProfileUnknown FanOutOutcome = "profile_unknown"
)

// Final reports whether an outcome is settled for its op: a non-final outcome
// is retried on the next pass.
func (o FanOutOutcome) Final() bool {
	return o != FanOutScopeUnresolved && o != FanOutNoSiteSource && o != FanOutTombstoned
}

// FanOutApplication is one recorded outcome.
type FanOutApplication struct {
	OpHash        string
	DesiredItemID string
	Outcome       FanOutOutcome
	Detail        string
	AppliedAt     time.Time
}

// FanOutApplications reads every recorded outcome, by op hash.
func (c *Catalog) FanOutApplications(ctx context.Context) (map[string]FanOutApplication, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT op_hash, COALESCE(desired_item_id, ''), outcome, detail, applied_at FROM catalog_op_applications`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading fan-out applications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]FanOutApplication{}
	for rows.Next() {
		var a FanOutApplication
		var at string
		if err := rows.Scan(&a.OpHash, &a.DesiredItemID, &a.Outcome, &a.Detail, &at); err != nil {
			return nil, fmt.Errorf("catalog: reading fan-out applications: %w", err)
		}
		a.AppliedAt = parseStamp(at)
		out[a.OpHash] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading fan-out applications: %w", err)
	}
	return out, nil
}

// RecordFanOutApplication upserts the outcome for an op and, when it is new or
// differs from the stored one, emits acquisition.fan_out_changed in the same
// transaction (invariant 7). An unchanged outcome writes nothing.
func (c *Catalog) RecordFanOutApplication(ctx context.Context, opHash, desiredItemID string, outcome FanOutOutcome, detail string) error {
	var want any
	if desiredItemID != "" {
		want = desiredItemID
	}
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var prevWant, prevOutcome, prevDetail string
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(desired_item_id, ''), outcome, detail FROM catalog_op_applications WHERE op_hash = ?`,
			opHash).Scan(&prevWant, &prevOutcome, &prevDetail)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case prevWant == desiredItemID && prevOutcome == string(outcome) && prevDetail == detail:
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO catalog_op_applications (op_hash, desired_item_id, outcome, detail, applied_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (op_hash) DO UPDATE SET
				desired_item_id = excluded.desired_item_id, outcome = excluded.outcome,
				detail = excluded.detail, applied_at = excluded.applied_at`,
			opHash, want, string(outcome), detail, sqlite.FormatTimestamp(c.clock.Now())); err != nil {
			return err
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeFanOutApplicationChanged, "catalog_op", opHash,
			map[string]any{
				"op_hash": opHash, "desired_item_id": desiredItemID,
				"outcome": string(outcome), "was": prevOutcome, "detail": detail,
			})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: recording fan-out application %s: %w", opHash, err)
	}
	c.events.Publish(pending...)
	return nil
}

// FanOutWant is the want an acquire op resolved to on this node.
type FanOutWant struct {
	DesiredItemID string
	// Created is whether the want was created by this call (false: it existed).
	Created bool
	// WorkCreated is whether the work was created by this call.
	WorkCreated bool
	// ProfileFallback is set when the op's profile name is unknown here and
	// the content type's default profile was used instead.
	ProfileFallback string
}

// errFanOutScopeUnresolved and errFanOutTombstoned are the control signals
// EnsureFanOutWant turns into outcomes.
var (
	errFanOutScopeUnresolved = errors.New("catalog: the op's edition or item is not materialised here")
	errFanOutTombstoned      = errors.New("catalog: the op's work is tombstoned here")
	errFanOutNoProfile       = errors.New("catalog: no profile for the op and no default for its content type")
)

// EnsureFanOutWant makes the work and want an acquire op names exist on this
// node, in one transaction, and returns the want. It converges on the op's
// (content_type, work_key) exactly as a scan or a want would — the work is
// created under THAT key, with the op's title and year, when absent — so an
// asset the grab ingests lands on the same work at both sites.
//
// The scope must already be materialised here: an edition by edition_key or an
// item by item_key under the work. If it is not, ErrFanOutScopeUnresolved is
// the caller's cue to try again later (a followed source's next poll, a scan).
func (c *Catalog) EnsureFanOutWant(ctx context.Context, op catalogop.Op) (FanOutWant, error) {
	if op.Kind != catalogop.OpAcquire || op.Acquire == nil {
		return FanOutWant{}, errors.New("catalog: not an acquire op")
	}
	a := op.Acquire
	ct := strings.ToLower(strings.TrimSpace(op.Target.ContentType))

	// The profile, by NAME. Unknown here means the sibling runs a profile this
	// site never created; the content type's seeded default is the standard a
	// want here would have had anyway (ADR-0082, ADR-0115).
	profileID, found, err := c.ProfileIDByName(ctx, a.Profile)
	if err != nil {
		return FanOutWant{}, err
	}
	var fallback string
	if !found {
		def := strategy.For(ct).DefaultProfile
		if def == "" {
			return FanOutWant{}, fmt.Errorf("%w: %q", errFanOutNoProfile, a.Profile)
		}
		profileID, found, err = c.ProfileIDByName(ctx, def)
		if err != nil {
			return FanOutWant{}, err
		}
		if !found {
			return FanOutWant{}, fmt.Errorf("%w: %q (default %q is not seeded)", errFanOutNoProfile, a.Profile, def)
		}
		fallback = def
	}

	var out FanOutWant
	out.ProfileFallback = fallback
	now := c.clock.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	var (
		item    desired.Item
		pending []events.Event
	)
	// Pass 1, one transaction: the work (created under the op's key when
	// absent), the scope, and the existing want if there is one.
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		// Tombstoned here means a person deleted it at this site, or the
		// sibling's delete arrived; the acquire loses (ADR-0073 remove-wins).
		var tomb int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM work_tombstones WHERE content_type = ? AND work_key = ?`,
			ct, op.Target.WorkKey).Scan(&tomb); {
		case err == nil:
			return errFanOutTombstoned
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var workID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM works WHERE content_type = ? AND work_key = ?`,
			ct, op.Target.WorkKey).Scan(&workID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, sortTitle := identification.Describe(ct, a.WorkTitle, a.Year)
			workID = uuid.Must(uuid.NewV7()).String()
			var year any
			if a.Year > 0 {
				year = a.Year
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO works (id, content_type, work_key, title, sort_title, year,
					attributes, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
				workID, ct, op.Target.WorkKey, identification.DisplayTitle(ct, a.WorkTitle), sortTitle, year, stamp, stamp); err != nil {
				return fmt.Errorf("catalog: creating work %s/%s for a fan-out want: %w", ct, op.Target.WorkKey, err)
			}
			ev, err := c.events.EmitTx(ctx, tx, events.TypeWorkCreated, "work", workID,
				map[string]any{"work_id": workID, "content_type": ct, "title": a.WorkTitle, "created_by": "fan-out"})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
			out.WorkCreated = true
		case err != nil:
			return err
		}

		item = desired.Item{
			WorkID: workID, QualityProfileID: profileID, Monitor: true,
			Reason: "fan-out from site " + a.Site + " (ADR-0119)",
		}
		switch {
		case a.Scope == catalogop.ScopeWork:
			item.Scope = desired.ScopeWork
		case strings.HasPrefix(a.Scope, catalogop.ScopeEditionPrefix):
			key := strings.TrimPrefix(a.Scope, catalogop.ScopeEditionPrefix)
			var id string
			switch err := tx.QueryRowContext(ctx,
				`SELECT id FROM editions WHERE work_id = ? AND edition_key = ?`, workID, key).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("%w: edition %q", errFanOutScopeUnresolved, key)
			case err != nil:
				return err
			}
			item.Scope, item.EditionID = desired.ScopeEdition, id
		case strings.HasPrefix(a.Scope, catalogop.ScopeItemPrefix):
			key := strings.TrimPrefix(a.Scope, catalogop.ScopeItemPrefix)
			var id string
			switch err := tx.QueryRowContext(ctx,
				`SELECT id FROM items WHERE work_id = ? AND item_key = ?`, workID, key).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("%w: item %q", errFanOutScopeUnresolved, key)
			case err != nil:
				return err
			}
			item.Scope, item.ItemID = desired.ScopeItem, id
		default:
			return fmt.Errorf("catalog: acquire scope %q", a.Scope)
		}
		if lang, ok := strings.CutPrefix(a.Aspect, catalogop.AspectSubtitlePrefix); ok {
			item.Aspect, item.Language = desired.AspectSubtitle, lang
		} else {
			item.Aspect = desired.AspectPrimary
		}
		if err := item.Validate(); err != nil {
			return err
		}
		id, err := existingWantTx(ctx, tx, item)
		if err != nil {
			return err
		}
		out.DesiredItemID = id
		return nil
	})
	if err != nil {
		return FanOutWant{}, err
	}
	c.events.Publish(pending...)
	if out.DesiredItemID != "" {
		return out, nil
	}
	// Pass 2: the want, through the one implementation every caller shares
	// (its own transaction, its resting state, its events). A duplicate here
	// is a concurrent creator; take theirs.
	rec, err := c.CreateDesiredItem(ctx, item)
	switch {
	case err == nil:
		out.DesiredItemID, out.Created = rec.Item.ID, true
		return out, nil
	case IsDuplicateWant(err):
		id, lookErr := existingWant(ctx, c.db.Reader(), item)
		if lookErr != nil {
			return FanOutWant{}, lookErr
		}
		out.DesiredItemID = id
		return out, nil
	default:
		return FanOutWant{}, err
	}
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func existingWantTx(ctx context.Context, tx *sql.Tx, item desired.Item) (string, error) {
	return existingWant(ctx, tx, item)
}

// existingWant finds the want over exactly this target, aspect and profile;
// "" when none. The same identity the desired_items unique index enforces.
func existingWant(ctx context.Context, q rowQuerier, item desired.Item) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM desired_items
		WHERE work_id = ? AND quality_profile_id = ?
		  AND COALESCE(edition_id, '') = ? AND COALESCE(item_id, '') = ?
		  AND aspect = ? AND language = ?`,
		item.WorkID, item.QualityProfileID, item.EditionID, item.ItemID,
		string(item.Aspect), item.Language).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("catalog: looking up a fan-out want: %w", err)
	}
	return id, nil
}

// IsFanOutScopeUnresolved reports an EnsureFanOutWant error that means "not
// yet": the edition or item is not materialised here.
func IsFanOutScopeUnresolved(err error) bool { return errors.Is(err, errFanOutScopeUnresolved) }

// IsFanOutTombstoned reports an EnsureFanOutWant error that means the work is
// tombstoned here.
func IsFanOutTombstoned(err error) bool { return errors.Is(err, errFanOutTombstoned) }

// IsFanOutNoProfile reports an EnsureFanOutWant error that means no profile
// could be resolved for the op.
func IsFanOutNoProfile(err error) bool { return errors.Is(err, errFanOutNoProfile) }
