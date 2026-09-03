package resources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Editing and removing a work (#428).
//
// Until now the only removal a client could express was per-asset
// (DELETE /assets/{id}) plus cancelling wants, so a work whose files had all
// been removed lingered in /works forever with nothing left in it — and a
// mistyped title could not be corrected at all. Both are ordinary library
// management, so both are `write` and not `admin`.

// WorkPatch is the body of PATCH /api/v1/works/{id}: the catalogue facts an
// operator may correct. Every field is a POINTER so that "set the year to
// nothing" and "do not touch the year" are different requests — with plain
// values they are the same zero and the second silently becomes the first.
type WorkPatch struct {
	Title       *string `json:"title"`
	Year        *int    `json:"year"`
	ContentType *string `json:"content_type"`
}

// patchWork corrects a work's title, year or content type.
//
// # work_key is deliberately NOT recomputed
//
// work_key is the normalised identity a rescan converges on (§11, M1-11), and
// it is derived from the title a SCANNER saw — not from the title a person
// prefers to read. Correcting a misspelt title must therefore leave the
// key alone: the files on disk still parse to the old key, and re-deriving it
// would make the very next scan create a SECOND work rather than converge on
// this one. The display title changes; the identity does not. sort_title does
// follow the title, because it is the listing's sort key and a renamed work
// that sorts under its old name is a bug a person can see.
func (a *API) patchWork(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body WorkPatch
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if body.Title == nil && body.Year == nil && body.ContentType == nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"a work patch must change something — title, year or content_type"))
		return
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) == "" {
		httpapi.Fail(w, r, problem.BadRequest("a work's title cannot be blank"))
		return
	}
	if body.Year != nil && (*body.Year < 0 || *body.Year > 9999) {
		httpapi.Fail(w, r, problem.BadRequest(
			fmt.Sprintf("a year of %d is not a year", *body.Year)))
		return
	}
	if body.ContentType != nil && !identification.IsContentType(*body.ContentType) {
		httpapi.Fail(w, r, problem.BadRequest(
			fmt.Sprintf("%q is not a content type this node knows", *body.ContentType)))
		return
	}

	var (
		updated Work
		ev      events.Event
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		before, err := scanWork(tx.QueryRowContext(r.Context(),
			`SELECT `+workColumns+` FROM works WHERE id = ?`, id))
		if err != nil {
			return err
		}

		after := before
		if body.Title != nil {
			// Stored as typed (trimmed), NOT run through the scanner's
			// DisplayTitle. That normaliser exists so a wanted work and a
			// scanned one read alike; applied to a correction it would quietly
			// discard the very thing a person meant — "Arrival (Extended)"
			// parses "(Extended)" as a release tag and drops it. A person
			// editing a title means it literally.
			after.Title = strings.TrimSpace(*body.Title)
		}
		if body.Year != nil {
			if *body.Year == 0 {
				after.Year = nil
			} else {
				y := int64(*body.Year)
				after.Year = &y
			}
		}
		if body.ContentType != nil {
			after.ContentType = *body.ContentType
		}
		// sort_title follows the title (and the content type it is normalised
		// under); work_key does not — see the doc comment.
		year := 0
		if after.Year != nil {
			year = int(*after.Year)
		}
		_, after.SortTitle = identification.Describe(after.ContentType, after.Title, year)
		after.UpdatedAt = a.now().UTC()

		var yearArg any
		if after.Year != nil {
			yearArg = *after.Year
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE works SET content_type = ?, title = ?, sort_title = ?, year = ?, updated_at = ?
			WHERE id = ?`,
			after.ContentType, after.Title, after.SortTitle, yearArg,
			after.UpdatedAt.Format(time.RFC3339Nano), id); err != nil {
			return err
		}
		updated = after

		ev, err = a.events.EmitTx(r.Context(), tx, events.TypeWorkUpdated, "work", id,
			map[string]any{
				"work_id":      id,
				"title":        after.Title,
				"content_type": after.ContentType,
				// The previous values, so the log answers "what was it called
				// before" without a subscriber having to have been listening.
				"previous_title":        before.Title,
				"previous_content_type": before.ContentType,
			})
		return err
	})
	if err != nil {
		a.failWorkWrite(w, r, err)
		return
	}
	a.events.Publish(ev)
	a.write(w, r, http.StatusOK, updated)
}

// deleteWork removes a work, its editions, its assets and its wants, and never
// touches a byte.
//
// # What "logical" means here (ADR-0018, invariant 8)
//
// The catalog rows go; the blobs stay. A blob is shared, expensive and
// unrecoverable, and the existing gc_blobs sweeper reclaims one no asset
// references any more after a grace window. Unlinking bytes inside a request
// handler is the version of this feature where a bug is not recoverable, which
// is exactly what ADR-0018 exists to prevent — so the event says
// `bytes_removed: false` in as many words.
//
// # Wants are cascade-cancelled; a SUBSCRIPTION refuses the delete
//
// The two look alike and are not. A want is a one-off intent — "get me this" —
// and deleting the thing it points at supersedes it, so the wants go with the
// work and each one's removal is emitted. A followed source is STANDING
// configuration that will keep producing wants, and silently dropping one
// because somebody tidied a work away is a surprise an operator cannot undo:
// deleting a followed work is refused with a 409 that names the subscription,
// and DELETE /followed-sources/{id} — which also decides what happens to the
// archive — is the explicit way to stop it.
func (a *API) deleteWork(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var (
		emitted   []events.Event
		followers int
	)
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		var title, contentType string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT title, content_type FROM works WHERE id = ?`, id).
			Scan(&title, &contentType); err != nil {
			return err
		}
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM follow_sources WHERE work_id = ?`, id).
			Scan(&followers); err != nil {
			return err
		}
		if followers > 0 {
			return errWorkIsFollowed
		}

		assets, err := assetsOfWork(r.Context(), tx, id)
		if err != nil {
			return err
		}
		wants, err := wantIDsOfWork(r.Context(), tx, id)
		if err != nil {
			return err
		}
		var editions int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT count(*) FROM editions WHERE work_id = ?`, id).Scan(&editions); err != nil {
			return err
		}

		// One statement. Every child of a work — editions, and through them
		// assets and consumption sessions; wants; items — is ON DELETE CASCADE,
		// so the database performs the removal it already knows how to perform
		// rather than this handler re-deriving the graph and eventually missing
		// a table somebody added later.
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM works WHERE id = ?`, id); err != nil {
			return err
		}

		// A subscriber that reacts to an asset disappearing must not have to
		// special-case "unless the whole work went", so each removed asset is
		// emitted exactly as DELETE /assets/{id} emits it — same type, same
		// payload, same bytes_removed: false.
		for _, as := range assets {
			ev, err := a.events.EmitTx(r.Context(), tx, events.TypeAssetDeleted, "asset", as.id,
				map[string]any{
					"asset_id":      as.id,
					"source_class":  as.sourceClass,
					"blob_hash":     as.blobHash,
					"bytes_removed": false,
					"reason":        "work_deleted",
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
					"target_type":     "work",
					"target_id":       id,
					"reason":          "work_deleted",
				})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
		}

		ev, err := a.events.EmitTx(r.Context(), tx, events.TypeWorkDeleted, "work", id,
			map[string]any{
				"work_id":      id,
				"title":        title,
				"content_type": contentType,
				"editions":     editions,
				"assets":       len(assets),
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
		a.failWorkWrite(w, r, err)
		return
	}
	for _, ev := range emitted {
		a.events.Publish(ev)
	}
	w.WriteHeader(http.StatusNoContent)
}

// errWorkIsFollowed is the refusal a followed work's deletion gets.
var errWorkIsFollowed = errors.New("resources: the work is still followed")

// removedAsset is what the delete needs to emit an asset's removal: exactly the
// fields DELETE /assets/{id} puts in its event.
type removedAsset struct {
	id          string
	sourceClass string
	blobHash    *string
}

// assetsOfWork lists the work's assets, joined through its editions.
func assetsOfWork(ctx context.Context, tx *sql.Tx, workID string) ([]removedAsset, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.source_class, a.blob_hash
		FROM assets a JOIN editions e ON e.id = a.edition_id
		WHERE e.work_id = ? ORDER BY a.id`, workID)
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

// wantIDsOfWork lists every want anchored to the work, at any scope.
func wantIDsOfWork(ctx context.Context, tx *sql.Tx, workID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM desired_items WHERE work_id = ? ORDER BY id`, workID)
	if err != nil {
		return nil, err
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

// failWorkWrite renders a work mutation's failure: the followed-work refusal is
// a 409 that says what to do about it, a duplicate identity is the 409 the
// (content_type, work_key) uniqueness produces, and everything else falls
// through to the shared mapping — where a missing work is a 404 rather than a
// 500.
func (a *API) failWorkWrite(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errWorkIsFollowed):
		httpapi.Fail(w, r, problem.Conflict(
			"the work is still followed — unfollow the source first "+
				"(DELETE /api/v1/followed-sources/{id}), which also decides what happens to its archive"))
	case isUniqueViolation(err):
		httpapi.Fail(w, r, problem.Conflict(
			"another work of that content type already has this work's identity"))
	default:
		a.fail(w, r, "work", err)
	}
}
