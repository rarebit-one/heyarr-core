package resources

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Removing an edition (#439 follow-up).
//
// #439 gave a client DELETE /works/{id} — the whole work and everything under
// it. An edition is the grouping between the two: a season of a series, a
// particular cut or pressing of a film. It is SCANNER-RECREATABLE — a rescan
// re-derives it from the files on disk — so removing one is ordinary library
// management, the same class as removing an asset or a work, and so `write`
// and not `admin`.

// deleteEdition removes an edition, its assets, its byte-less items and the
// wants scoped beneath it, and never touches a byte.
//
// # What "logical" means here (ADR-0018, invariant 8)
//
// Identical to the work delete: the catalog rows go, the blobs stay. A blob is
// shared, expensive and unrecoverable, and the existing gc_blobs sweeper
// reclaims one no asset references any more after a grace window. The parent
// work is untouched — an edition is a subordinate grouping, and a rescan will
// re-create it from the files that remain. The event says
// `bytes_removed: false` in as many words.
//
// # A followed work refuses the delete
//
// A follow source (00040) anchors to a WORK, not an edition: it is standing
// configuration that keeps projecting that work's items — and its seasons are
// exactly the editions this route would remove. Tidying an edition out from
// under a live subscription is the same surprise the work delete refuses:
// deleting an edition whose work is still followed answers 409 naming the fix
// (DELETE /followed-sources/{id}, which also decides what happens to the
// archive) rather than letting the next poll silently re-materialise what was
// just removed. A want is the opposite — a one-off intent that removing its
// target supersedes — so the wants beneath the edition are cascade-cancelled
// and each one's removal is emitted.
func (a *API) deleteEdition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		emitted   []events.Event
		followers int
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		var workID, label, editionType string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT work_id, label, edition_type FROM editions WHERE id = ?`, id).
			Scan(&workID, &label, &editionType); err != nil {
			return err
		}
		// The subscription anchors to the work, so the check is on the work
		// this edition belongs to.
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM follow_sources WHERE work_id = ?`, workID).
			Scan(&followers); err != nil {
			return err
		}
		if followers > 0 {
			return errEditionWorkIsFollowed
		}

		assets, err := assetsOfEdition(r.Context(), tx, id)
		if err != nil {
			return err
		}
		wants, err := wantIDsOfEdition(r.Context(), tx, id)
		if err != nil {
			return err
		}
		var items int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM items WHERE edition_id = ?`, id).Scan(&items); err != nil {
			return err
		}

		// One statement. Every child of an edition — its assets and their
		// consumption sessions, its items, and the wants scoped to the edition
		// or to one of its items — is ON DELETE CASCADE, so the database
		// performs the removal it already knows how to perform rather than this
		// handler re-deriving the graph and eventually missing a table somebody
		// added later.
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM editions WHERE id = ?`, id); err != nil {
			return err
		}

		// A subscriber that reacts to an asset disappearing must not have to
		// special-case "unless the whole edition went", so each removed asset
		// is emitted exactly as DELETE /assets/{id} emits it — same type, same
		// payload, same bytes_removed: false.
		for _, as := range assets {
			ev, err := a.events.EmitTx(r.Context(), tx, events.TypeAssetDeleted, "asset", as.id,
				map[string]any{
					"asset_id":      as.id,
					"source_class":  as.sourceClass,
					"blob_hash":     as.blobHash,
					"bytes_removed": false,
					"reason":        "edition_deleted",
				})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
		}
		for _, want := range wants {
			ev, err := a.events.EmitTx(r.Context(), tx, events.TypeDesiredRemoved,
				"desired_item", want, map[string]any{
					"desired_item_id": want,
					"target_type":     "edition",
					"target_id":       id,
					"reason":          "edition_deleted",
				})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
		}

		ev, err := a.events.EmitTx(r.Context(), tx, events.TypeEditionDeleted, "edition", id,
			map[string]any{
				"edition_id":   id,
				"work_id":      workID,
				"label":        label,
				"edition_type": editionType,
				"assets":       len(assets),
				"items":        items,
				"wants":        len(wants),
				// The whole point of ADR-0018, and the first question anyone
				// reading the log will have.
				"bytes_removed": false,
			})
		if err != nil {
			return err
		}
		emitted = append(emitted, ev)
		return nil
	})
	if err != nil {
		a.failEditionWrite(w, r, err)
		return
	}
	for _, ev := range emitted {
		a.events.Publish(ev)
	}
	w.WriteHeader(http.StatusNoContent)
}

// errEditionWorkIsFollowed is the refusal a followed work's edition gets.
var errEditionWorkIsFollowed = errors.New("resources: the edition's work is still followed")

// assetsOfEdition lists the edition's assets — exactly the fields
// DELETE /assets/{id} needs to emit each one's removal.
func assetsOfEdition(ctx context.Context, tx *sql.Tx, editionID string) ([]removedAsset, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, source_class, blob_hash
		FROM assets WHERE edition_id = ? ORDER BY id`, editionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []removedAsset
	for rows.Next() {
		var as removedAsset
		var hash sql.NullString
		if err := rows.Scan(&as.id, &as.sourceClass, &hash); err != nil {
			return nil, err
		}
		as.blobHash = nullString(hash)
		out = append(out, as)
	}
	return out, rows.Err()
}

// wantIDsOfEdition lists every want the edition delete cascade-cancels: the
// edition-scoped wants pointed straight at it, and the item-scoped wants
// pointed at an item that belongs to it (00041). A work-scoped want over the
// parent work is NOT one of them — its target, the work, still exists.
func wantIDsOfEdition(ctx context.Context, tx *sql.Tx, editionID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM desired_items
		WHERE edition_id = ?
		   OR item_id IN (SELECT id FROM items WHERE edition_id = ?)
		ORDER BY id`, editionID, editionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var wantID string
		if err := rows.Scan(&wantID); err != nil {
			return nil, err
		}
		out = append(out, wantID)
	}
	return out, rows.Err()
}

// failEditionWrite renders an edition mutation's failure: the followed-work
// refusal is a 409 that says what to do about it, and everything else falls
// through to the shared mapping — where a missing edition is a 404 rather than
// a 500.
func (a *API) failEditionWrite(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errEditionWorkIsFollowed):
		httpapi.Fail(w, r, problem.Conflict(
			"the edition's work is still followed — unfollow the source first "+
				"(DELETE /api/v1/followed-sources/{id}), which also decides what happens to its archive"))
	default:
		a.fail(w, r, "edition", err)
	}
}
