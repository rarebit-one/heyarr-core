package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// ---------------------------------------------------------------------------
// Works
// ---------------------------------------------------------------------------

const workColumns = `id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at`

func scanWork(row interface{ Scan(...any) error }) (Work, error) {
	var w Work
	var year sql.NullInt64
	var attributes, created, updated string
	if err := row.Scan(&w.ID, &w.ContentType, &w.WorkKey, &w.Title, &w.SortTitle,
		&year, &attributes, &created, &updated); err != nil {
		return Work{}, err
	}
	w.Year = nullInt(year)
	w.Attributes = json.RawMessage(attributes)
	w.CreatedAt = parseTime(created)
	w.UpdatedAt = parseTime(updated)
	return w, nil
}

// listWorks pages the catalog.
//
// The sort key is (sort_title, id). sort_title alone is not unique — a library
// with two editions of the same title is the normal case, not a contrived one —
// and a non-unique keyset boundary is exactly as lossy as OFFSET: rows sharing
// the boundary value are skipped or repeated. The id breaks the tie.
func (a *API) listWorks(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "works", 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if ct := r.URL.Query().Get("content_type"); ct != "" {
		where = append(where, "content_type = ?")
		args = append(args, ct)
	}
	if lib := r.URL.Query().Get("library_id"); lib != "" {
		// "Works with something of theirs in this library". Expressed as EXISTS
		// rather than a join so that a work with twenty assets in the library
		// is one row, not twenty.
		where = append(where, `EXISTS (SELECT 1 FROM editions e
			JOIN assets a ON a.edition_id = e.id
			WHERE e.work_id = works.id AND a.library_id = ?)`)
		args = append(args, lib)
	}
	if term := r.URL.Query().Get("q"); term != "" {
		where = append(where, `sort_title LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(term))
	}
	if q.cursor != nil {
		where = append(where, "(sort_title, id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + workColumns + ` FROM works WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY sort_title ASC, id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var works []Work
	for rows.Next() {
		work, err := scanWork(rows)
		if err != nil {
			a.fail(w, r, "work", err)
			return
		}
		works = append(works, work)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "work", err)
		return
	}

	a.write(w, r, http.StatusOK, newPage(works, q.limit,
		func(x Work) []string { return []string{x.SortTitle, x.ID} }, "works"))
}

func (a *API) getWork(w http.ResponseWriter, r *http.Request) {
	row := a.reader.QueryRowContext(r.Context(),
		`SELECT `+workColumns+` FROM works WHERE id = ?`, chi.URLParam(r, "id"))
	work, err := scanWork(row)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	a.write(w, r, http.StatusOK, work)
}

// ---------------------------------------------------------------------------
// Editions
// ---------------------------------------------------------------------------

const editionColumns = `id, work_id, label, edition_type, language, attributes, created_at`

func scanEdition(row interface{ Scan(...any) error }) (Edition, error) {
	var e Edition
	var language sql.NullString
	var attributes, created string
	if err := row.Scan(&e.ID, &e.WorkID, &e.Label, &e.EditionType, &language,
		&attributes, &created); err != nil {
		return Edition{}, err
	}
	e.Language = nullString(language)
	e.Attributes = json.RawMessage(attributes)
	e.CreatedAt = parseTime(created)
	return e, nil
}

func (a *API) getEdition(w http.ResponseWriter, r *http.Request) {
	row := a.reader.QueryRowContext(r.Context(),
		`SELECT `+editionColumns+` FROM editions WHERE id = ?`, chi.URLParam(r, "id"))
	edition, err := scanEdition(row)
	if err != nil {
		a.fail(w, r, "edition", err)
		return
	}
	a.write(w, r, http.StatusOK, edition)
}

// ---------------------------------------------------------------------------
// Assets
// ---------------------------------------------------------------------------

const assetColumns = `id, edition_id, library_id, source_class, blob_hash, source_path,
	role, filename, mime, identification_source, missing_since, created_at, updated_at`

func scanAsset(row interface{ Scan(...any) error }) (Asset, error) {
	var as Asset
	var library, blob, path, filename, mime sql.NullString
	var missing sql.NullString
	var created, updated string
	if err := row.Scan(&as.ID, &as.EditionID, &library, &as.SourceClass, &blob, &path,
		&as.Role, &filename, &mime, &as.IdentificationSource, &missing,
		&created, &updated); err != nil {
		return Asset{}, err
	}
	as.LibraryID = nullString(library)
	as.BlobHash = nullString(blob)
	as.SourcePath = nullString(path)
	as.Filename = nullString(filename)
	as.MIME = nullString(mime)
	as.MissingSince = parseNullTime(missing)
	as.CreatedAt = parseTime(created)
	as.UpdatedAt = parseTime(updated)
	return as, nil
}

// listAssets pages the files. The sort key is the id alone, which is a UUIDv7
// and therefore already in creation order (ADR-0017) — a second column would
// buy nothing and cost an index.
func (a *API) listAssets(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "assets", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	state, err := oneOf(r, "state", "present", "missing")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if lib := r.URL.Query().Get("library_id"); lib != "" {
		where = append(where, "library_id = ?")
		args = append(args, lib)
	}
	if ct := r.URL.Query().Get("content_type"); ct != "" {
		where = append(where, `EXISTS (SELECT 1 FROM editions e
			JOIN works k ON k.id = e.work_id
			WHERE e.id = assets.edition_id AND k.content_type = ?)`)
		args = append(args, ct)
	}
	switch state {
	case "present":
		where = append(where, "missing_since IS NULL")
	case "missing":
		where = append(where, "missing_since IS NOT NULL")
	}
	if q.cursor != nil {
		where = append(where, "id > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + assetColumns + ` FROM assets WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var assets []Asset
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			a.fail(w, r, "asset", err)
			return
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "asset", err)
		return
	}

	a.write(w, r, http.StatusOK, newPage(assets, q.limit,
		func(x Asset) []string { return []string{x.ID} }, "assets"))
}

func (a *API) getAsset(w http.ResponseWriter, r *http.Request) {
	asset, err := a.loadAsset(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	a.write(w, r, http.StatusOK, asset)
}

func (a *API) loadAsset(ctx context.Context, id string) (Asset, error) {
	row := a.reader.QueryRowContext(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = ?`, id)
	return scanAsset(row)
}

// deleteAsset removes the catalog row and never touches a byte.
//
// This is what ADR-0018 means by logical: the Asset stops existing, the Blob
// does not, and a later gc_blobs sweep reclaims blobs that no Asset references
// once a grace window has passed. Unlinking bytes inside a request handler is
// the version of this feature where a bug is unrecoverable.
func (a *API) deleteAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var event events.Event
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		var blobHash sql.NullString
		var sourceClass string
		err := tx.QueryRowContext(r.Context(),
			`SELECT source_class, blob_hash FROM assets WHERE id = ?`, id).
			Scan(&sourceClass, &blobHash)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM assets WHERE id = ?`, id); err != nil {
			return err
		}
		// Emitted inside the transaction so the deletion and its event commit
		// together (invariant 7). Publishing happens after the commit, because
		// a subscriber must never see an event a rollback then erases.
		event, err = a.events.EmitTx(r.Context(), tx, events.TypeAssetDeleted, "asset", id,
			map[string]any{
				"asset_id":     id,
				"source_class": sourceClass,
				"blob_hash":    nullString(blobHash),
				// Said explicitly because it is the whole point of ADR-0018 and
				// the first question anyone reading the event log will have.
				"bytes_removed": false,
			})
		return err
	})
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	a.events.Publish(event)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Blobs — metadata only. The bytes are ADR-0013's separate contract.
// ---------------------------------------------------------------------------

func (a *API) getBlob(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	var b Blob
	var mime sql.NullString
	var state string
	var firstSeen string
	// §16's three-way answer, derived in the same read that fetches the blob
	// (M5-03). It is a SELECT and nothing else: asking whether a blob has a
	// chunk manifest must never produce one, and a GET that generated a
	// manifest as a side effect would be the most convenient possible place to
	// break that rule (ADR-0034).
	err := a.reader.QueryRowContext(r.Context(), `
		SELECT b.hash, b.size, b.mime, b.first_seen_at,
		       CASE
		           WHEN m.blob_hash IS NOT NULL              THEN 'present'
		           WHEN b.chunking_exempt_reason IS NOT NULL THEN 'not_required'
		           ELSE 'undecided'
		       END
		FROM blobs b
		LEFT JOIN chunk_manifests m ON m.blob_hash = b.hash
		WHERE b.hash = ?`, hash).
		Scan(&b.Hash, &b.Size, &mime, &firstSeen, &state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The 404 says which hash so an operator can tell "I typed it
			// wrong" from "this instance does not have it" — the hash is not a
			// secret, it is the identity the client just sent.
			httpapi.Fail(w, r, problem.NotFound("no blob is recorded with the hash "+hash))
			return
		}
		a.fail(w, r, "blob", err)
		return
	}
	b.MIME = nullString(mime)
	if b.ChunkManifest, err = manifests.ParseState(state); err != nil {
		a.fail(w, r, "blob", err)
		return
	}
	b.Chunked = b.ChunkManifest.HasManifest()
	b.FirstSeenAt = parseTime(firstSeen)
	a.write(w, r, http.StatusOK, b)
}

// ---------------------------------------------------------------------------
// A work's files (#429)
// ---------------------------------------------------------------------------

// listWorkAssets pages the assets belonging to one work, joined through its
// editions and with the blob facts inlined (#429).
//
// The sort key is the asset id alone — a UUIDv7, and therefore already in
// creation order (ADR-0017) — which is the key /assets pages by, so the two
// listings order identically.
//
// An unknown work is a 404 and never an empty page. A work with no files and a
// work that does not exist are different answers, and a client cannot tell them
// apart if both are `{"items": []}`: it is the difference between "add
// something" and "you asked for the wrong thing".
func (a *API) listWorkAssets(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	q, err := parseQuery(r, "work-assets", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	state, err := oneOf(r, "state", "present", "missing")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	var exists int
	if err := a.reader.QueryRowContext(r.Context(),
		`SELECT 1 FROM works WHERE id = ?`, id).Scan(&exists); err != nil {
		a.fail(w, r, "work", err)
		return
	}

	where := []string{"e.work_id = ?"}
	args := []any{id}
	switch state {
	case "present":
		where = append(where, "assets.missing_since IS NULL")
	case "missing":
		where = append(where, "assets.missing_since IS NOT NULL")
	}
	if q.cursor != nil {
		where = append(where, "assets.id > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	// The blobs join is LEFT because a `linked` asset has no blob at all
	// (ADR-0020): an INNER join would silently drop every linked file from a
	// work's listing, which is the one place a person is counting the files.
	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + prefixed(assetColumns, "assets") + `, e.label, e.edition_type, b.size, b.mime
		FROM assets
		JOIN editions e ON e.id = assets.edition_id
		LEFT JOIN blobs b ON b.hash = assets.blob_hash
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY assets.id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var assets []WorkAsset
	for rows.Next() {
		item, err := scanWorkAsset(rows)
		if err != nil {
			a.fail(w, r, "asset", err)
			return
		}
		assets = append(assets, item)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "asset", err)
		return
	}

	a.write(w, r, http.StatusOK, newPage(assets, q.limit,
		func(x WorkAsset) []string { return []string{x.ID} }, "work-assets"))
}

// scanWorkAsset reads an asset row followed by the four joined columns.
//
// It reuses scanAsset for the asset half rather than restating thirteen
// columns: a column added to assetColumns is then scanned here too, instead of
// this listing silently misaligning the day the asset shape grows.
func scanWorkAsset(rows *sql.Rows) (WorkAsset, error) {
	var item WorkAsset
	var size sql.NullInt64
	var blobMIME sql.NullString
	asset, err := scanAsset(appendedScan{rows: rows, extra: []any{
		&item.EditionLabel, &item.EditionType, &size, &blobMIME,
	}})
	if err != nil {
		return WorkAsset{}, err
	}
	item.Asset = asset
	if size.Valid {
		v := size.Int64
		item.BlobSize = &v
	}
	item.BlobMIME = nullString(blobMIME)
	return item, nil
}

// appendedScan lets a scanner written for one row shape read a WIDER row: it
// passes the caller's destinations through and appends its own. That is what
// keeps scanAsset the single definition of how an asset row is read.
type appendedScan struct {
	rows  *sql.Rows
	extra []any
}

func (a appendedScan) Scan(dest ...any) error {
	return a.rows.Scan(append(dest, a.extra...)...)
}

// prefixed qualifies a bare column list with a table alias, so the shared
// assetColumns constant can be used in a join without every column becoming
// ambiguous. It is a build-time constant fold over a literal, never over input.
func prefixed(columns, table string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = table + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
