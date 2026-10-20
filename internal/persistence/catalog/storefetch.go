package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// DueStoreFetch is one game want the store beat should hand to a store
// (ADR-0120).
type DueStoreFetch struct {
	DesiredItemID string
	// WorkID is the game. One fetch serves every want for it.
	WorkID string
	// Store names the store the game is fetched from; today only "gog".
	Store string
	// GameName is the store's own name for the game (the work's external id).
	GameName string
	// Title is the want's work title, for logs and the release candidate.
	Title string
	// Fruitless is the backoff exponent so far.
	Fruitless int
}

// DueStoreFetches lists game wants due a store fetch: resting, not yet
// satisfied (whether or not some bytes are already held), whose Work names its game on a store, and whose
// store_fetch_schedule says they are due. A want with no external id for any
// store is not listed — there is nothing a store could be asked for — and is
// left for an owned-library sync to name.
func (c *Catalog) DueStoreFetches(ctx context.Context, now time.Time, limit int) ([]DueStoreFetch, error) {
	if limit <= 0 {
		return nil, nil
	}
	//nolint:gosec // the concatenated fragments are storeFetchBackoff's literal table and columns; values are bound
	//
	// One representative want per game, chosen BEFORE the limit (the most overdue,
	// then by id): a game wanted under three profiles must take one slot of the
	// pass, not three, or a backfill would start one game per pass.
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT id, work_id, game, title, fruitless FROM (
		SELECT d.id, d.work_id, gog.value AS game, w.title, coalesce(s.fruitless, 0) AS fruitless,
		       `+storeFetchBackoff.dueOrder()+` AS due,
		       row_number() OVER (PARTITION BY d.work_id
		                          ORDER BY `+storeFetchBackoff.dueOrder()+`, d.id) AS pick
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		JOIN acquisition_state a ON a.desired_item_id = d.id
		JOIN external_ids gog
		  ON gog.entity_type = 'work' AND gog.entity_id = d.work_id AND gog.source = 'gog'
		`+storeFetchBackoff.leftJoin("d.id")+`
		WHERE w.content_type = 'game'
		  AND d.aspect != 'subtitle'
		  AND a.phase = 'idle'
		  -- A want fan-out created fetches its sibling's exact release or nothing
		  -- (ADR-0119): no scheduler here may drive it to another.
		  AND NOT EXISTS (SELECT 1 FROM fan_out_wants f WHERE f.desired_item_id = d.id)
		  -- Managed is fine: a game holding scanned files, or a package missing a
		  -- part, holds bytes that do not satisfy it, and the verified package is
		  -- what it lacks (ADR-0120).
		  AND a.content != 'satisfied'
		  -- Another want for the same game already has a fetch in flight: one
		  -- transfer serves the game, and its ingest satisfies every want for it.
		  AND NOT EXISTS (
		        SELECT 1 FROM desired_items o
		        JOIN acquisition_state oa ON oa.desired_item_id = o.id
		        WHERE o.work_id = d.work_id AND o.id != d.id AND oa.phase != 'idle')
		  -- Nor while another want for it is backing off: the backoff is the
		  -- game's, not one want's, or a second profile's want — which has no
		  -- schedule row of its own — would refetch the game the next pass.
		  AND NOT EXISTS (
		        SELECT 1 FROM desired_items o
		        JOIN store_fetch_schedule os ON os.desired_item_id = o.id
		        WHERE o.work_id = d.work_id AND o.id != d.id AND os.next_attempt_at > ?)
		  AND `+storeFetchBackoff.dueWhere()+`
		) WHERE pick = 1
		ORDER BY due, id
		LIMIT ?`, sqlite.FormatTimestamp(now), sqlite.FormatTimestamp(now), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing game wants due a store fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueStoreFetch
	for rows.Next() {
		d := DueStoreFetch{Store: "gog"}
		if err := rows.Scan(&d.DesiredItemID, &d.WorkID, &d.GameName, &d.Title, &d.Fruitless); err != nil {
			return nil, fmt.Errorf("catalog: reading game wants due a store fetch: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StrandedStoreSelections lists game wants whose store release was selected but
// that have no grab in flight, and have been that way since before olderThan:
// the beat moved the want to SELECTED, then the grab was never queued (a
// shutdown between the two, a queue error) or was lost. Nothing else would ever
// move such a want — the store beat looks only at resting wants — so the beat
// re-queues their grab, which the grab's dedupe key makes safe to repeat.
func (c *Catalog) StrandedStoreSelections(ctx context.Context, olderThan time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT d.id
		FROM desired_items d
		JOIN works w ON w.id = d.work_id
		JOIN acquisition_state a ON a.desired_item_id = d.id
		JOIN release_candidates rc
		  ON rc.desired_item_id = d.id AND rc.selected = 1 AND rc.provider = 'gog'
		WHERE w.content_type = 'game'
		  AND a.phase = 'selected'
		  AND a.phase_entered_at <= ?
		  AND NOT EXISTS (
		        SELECT 1 FROM jobs j
		        WHERE j.dedupe_key = 'grab:' || d.id AND j.state IN ('pending', 'leased'))
		ORDER BY a.phase_entered_at, d.id
		LIMIT ?`, sqlite.FormatTimestamp(olderThan), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing stranded store selections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RecordStoreFetchAttempt claims one store fetch for a want: it records the
// attempt and when the next is due, but only while the want is still due, so
// two beats racing the same want start one fetch between them. It reports
// whether this caller won.
//
// The attempt is recorded BEFORE the fetch starts, and counts as fruitless
// until the want is satisfied, at which point it leaves the due query. A fetch
// that fails therefore backs off without anyone having to remember to record
// the failure — the property a fetch that can take a day needs.
func (c *Catalog) RecordStoreFetchAttempt(
	ctx context.Context, desiredItemID string, fruitless int, now, next time.Time,
) (bool, error) {
	if desiredItemID == "" {
		return false, fmt.Errorf("catalog: recording a store fetch attempt needs a want")
	}
	won, err := storeFetchBackoff.record(ctx, c.db, backoffAttempt{
		subject: desiredItemID, fruitless: fruitless, now: now, next: next, onlyIfDue: true,
	})
	if err != nil {
		return false, fmt.Errorf("catalog: recording a store fetch attempt: %w", err)
	}
	return won, nil
}

// clearStoreFetchSchedule ends a want's store fetch backoff. Bookkeeping, not a
// state transition, so no event (as with every backoff write).
func (c *Catalog) clearStoreFetchSchedule(ctx context.Context, desiredItemID string) error {
	if _, err := c.db.Writer().ExecContext(ctx,
		`DELETE FROM store_fetch_schedule WHERE desired_item_id = ?`, desiredItemID); err != nil {
		return fmt.Errorf("catalog: clearing the store fetch schedule of %s: %w", desiredItemID, err)
	}
	return nil
}

// VerifiedStorePackage is what a store vouched for about a package it fetched.
type VerifiedStorePackage struct {
	Store string
	// DRMFree is recorded only when HasDRMFree: a store that does not say
	// leaves the attribute absent, never false (§63).
	DRMFree    bool
	HasDRMFree bool
	// Blobs is every file the store vouched for, as one package: the base
	// game and its DLC together. It is recorded on each edition the package
	// filled, and the store facts count only while all of it is held
	// (assetsAndPackagesForWant), and placement only once all of it is
	// replicated (acquisition.EvaluatePackagePlacement).
	Blobs []string
}

// RecordVerifiedStoreEditions writes a verified store package's facts onto the
// editions it filled (ADR-0118 §2, ADR-0120). It is the ONLY writer of store
// facts on an edition: a scanned filename describes one file, and a package is
// only known to be whole once the store has checked every file of it.
//
// Idempotent, as ingest is re-run: an edition already carrying these facts is
// left alone and emits nothing; one that changes emits
// content.edition.verified in the same transaction (invariant 7).
func (c *Catalog) RecordVerifiedStoreEditions(ctx context.Context, editionIDs []string, pkg VerifiedStorePackage) error {
	if pkg.Store == "" {
		return fmt.Errorf("catalog: a verified store package needs its store")
	}
	blobs := uniqueSorted(pkg.Blobs)
	if len(blobs) == 0 {
		return fmt.Errorf("catalog: a verified store package needs the files the store vouched for")
	}
	ids := uniqueSorted(editionIDs)
	if len(ids) == 0 {
		return nil
	}
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = pending[:0]
		for _, id := range ids {
			var workID, raw string
			err := tx.QueryRowContext(ctx,
				`SELECT work_id, attributes FROM editions WHERE id = ?`, id).Scan(&workID, &raw)
			if err != nil {
				return fmt.Errorf("catalog: reading edition %s: %w", id, err)
			}
			attrs := map[string]any{}
			if raw != "" {
				if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
					return fmt.Errorf("catalog: decoding edition %s attributes: %w", id, err)
				}
			}
			changed := attrs["store"] != pkg.Store
			attrs["store"] = pkg.Store
			if pkg.HasDRMFree {
				if attrs["drm_free"] != pkg.DRMFree {
					changed = true
				}
				attrs["drm_free"] = pkg.DRMFree
			}
			if !sameStrings(attrs["package_blobs"], blobs) {
				changed = true
			}
			attrs["package_blobs"] = blobs
			if !changed {
				continue
			}
			encoded, err := json.Marshal(attrs)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE editions SET attributes = ? WHERE id = ?`, string(encoded), id); err != nil {
				return fmt.Errorf("catalog: recording edition %s as verified: %w", id, err)
			}
			payload := map[string]any{
				"edition_id": id, "work_id": workID, "store": pkg.Store, "package_files": len(blobs),
			}
			if pkg.HasDRMFree {
				payload["drm_free"] = pkg.DRMFree
			}
			ev, err := c.events.EmitTx(ctx, tx, events.TypeEditionVerified, "edition", id, payload)
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, ev := range pending {
		c.events.Publish(ev)
	}
	return nil
}

// sameStrings reports whether a decoded JSON value is exactly this string list.
func sameStrings(v any, want []string) bool {
	got, ok := v.([]any)
	if !ok || len(got) != len(want) {
		return false
	}
	for i := range got {
		if s, ok := got[i].(string); !ok || s != want[i] {
			return false
		}
	}
	return true
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
