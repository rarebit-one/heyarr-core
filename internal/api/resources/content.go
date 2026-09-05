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
	"github.com/rarebit-one/heyarr-core/internal/guest"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// guestAssetFilter is the guest content boundary (ADR-0074) as a SQL predicate.
//
// When the caller is a Guest, an asset listing is restricted to the
// guest-visible source classes — the same allowlist guest.Visible enforces per
// item — pushed into the WHERE clause so keyset pagination stays exact (dropping
// rows from an already-fetched page would let a page of hidden assets truncate
// the listing). For any other caller it adds nothing.
//
// col is the column reference to filter on, so a query that aliases the assets
// table (`assets.source_class`) and one that does not (`source_class`) can share
// this. It has nothing to hide today — every asset written is `managed` — but it
// is where the vault boundary bites once personal content exists.
func guestAssetFilter(r *http.Request, col string) (string, []any) {
	id, ok := httpapi.IdentityFrom(r.Context())
	if !ok || !id.Guest {
		return "", nil
	}
	classes := guest.VisibleClasses()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(classes)), ",")
	args := make([]any, len(classes))
	for i, c := range classes {
		args[i] = c
	}
	return col + " IN (" + placeholders + ")", args
}

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
// The default sort key is (sort_title, id). sort_title alone is not unique — a
// library with two editions of the same title is the normal case, not a
// contrived one — and a non-unique keyset boundary is exactly as lossy as
// OFFSET: rows sharing the boundary value are skipped or repeated. The id
// breaks the tie. `sort=recent` keys on (created_at DESC, id DESC) under its
// own cursor collection, so a title-order cursor is refused there rather than
// read as a position in a different order.
//
// `include=artwork,primary_asset` adds the browse embeds (ADR-0075) to each
// row; without it the rows are plain works, byte-for-byte as before.
func (a *API) listWorks(w http.ResponseWriter, r *http.Request) {
	sortBy, err := oneOf(r, "sort", "title", "recent")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	collection := "works"
	if sortBy == "recent" {
		collection = "works-recent"
	}
	q, err := parseQuery(r, collection, 2)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	include, err := parseInclude(r, "artwork", "primary_asset")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	year, err := parseIntFilter(r, "year")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	yearFrom, err := parseIntFilter(r, "year_from")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	yearTo, err := parseIntFilter(r, "year_to")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	head, args := cardQuery(r, include["artwork"], include["primary_asset"])
	where := []string{"1 = 1"}
	if ct := r.URL.Query().Get("content_type"); ct != "" {
		where = append(where, "works.content_type = ?")
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
		where = append(where, `works.sort_title LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(term))
	}
	if year != nil {
		where = append(where, "works.year = ?")
		args = append(args, *year)
	}
	if yearFrom != nil {
		where = append(where, "works.year >= ?")
		args = append(args, *yearFrom)
	}
	if yearTo != nil {
		where = append(where, "works.year <= ?")
		args = append(args, *yearTo)
	}
	// The grouping facets (ADR-0075): an artist's albums, an author's books.
	// Exact match on the attribute the scanner wrote, which is what the
	// grouping reads hand back as the key.
	if artist := r.URL.Query().Get("artist"); artist != "" {
		where = append(where, "json_extract(works.attributes, '$.artist') = ?")
		args = append(args, artist)
	}
	if author := r.URL.Query().Get("author"); author != "" {
		where = append(where, "json_extract(works.attributes, '$.author') = ?")
		args = append(args, author)
	}
	order := ` ORDER BY works.sort_title ASC, works.id ASC`
	if sortBy == "recent" {
		order = ` ORDER BY works.created_at DESC, works.id DESC`
		if q.cursor != nil {
			where = append(where, "(works.created_at, works.id) < (?, ?)")
			args = append(args, q.cursor[0], q.cursor[1])
		}
	} else if q.cursor != nil {
		where = append(where, "(works.sort_title, works.id) > (?, ?)")
		args = append(args, q.cursor[0], q.cursor[1])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := head + ` WHERE ` + strings.Join(where, " AND ") + order + ` LIMIT ?`

	//nolint:gosec // see above: literal fragments only, every value bound
	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var cards []cardRow
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			a.fail(w, r, "work", err)
			return
		}
		cards = append(cards, card)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "work", err)
		return
	}

	// The cursor is derived from the cards, whichever shape is rendered: the
	// recent order keys on the stored created_at text, which only the card row
	// carries (re-rendering a parsed time can differ from the stored text, and
	// a keyset boundary must be the stored text).
	next := ""
	if len(cards) > q.limit {
		cards = cards[:q.limit]
		last := cards[len(cards)-1]
		if sortBy == "recent" {
			next = encodeCursor(collection, last.createdRaw, last.ID)
		} else {
			next = encodeCursor(collection, last.SortTitle, last.ID)
		}
	}
	if len(include) == 0 {
		works := make([]Work, 0, len(cards))
		for _, c := range cards {
			works = append(works, c.Work)
		}
		a.write(w, r, http.StatusOK, page[Work]{Items: works, NextCursor: next})
		return
	}
	out := make([]WorkCard, 0, len(cards))
	for _, c := range cards {
		out = append(out, WorkCard{
			Work:         c.Work,
			Artwork:      embed[ArtworkRef]{included: include["artwork"], value: c.artwork},
			PrimaryAsset: embed[PrimaryAssetRef]{included: include["primary_asset"], value: c.primary},
		})
	}
	a.write(w, r, http.StatusOK, page[WorkCard]{Items: out, NextCursor: next})
}

func (a *API) getWork(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	head, args := cardQuery(r, true, true)
	args = append(args, id)
	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	row := a.reader.QueryRowContext(r.Context(), head+` WHERE works.id = ?`, args...)
	card, err := scanCard(row)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	ids, err := a.externalIDs(r.Context(), "work", id)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	a.write(w, r, http.StatusOK, WorkDetail{
		Work: card.Work, ExternalIDs: ids, Artwork: card.artwork, PrimaryAsset: card.primary,
	})
}

// externalIDs projects the external_ids rows for one entity (ADR-0050, #431).
//
// Read-only, and empty rather than absent when nothing matches: ADR-0025's
// "a missing capability degrades to no match, never an error" applies to a
// catalogue identifier the same way, so a work nobody has reconciled yet
// answers `{}` and a caller can probe cheaply.
//
// The rows are ordered by (source, value) and the FIRST value for a source
// wins. The schema's UNIQUE (source, value, entity_type) does not forbid two
// tmdb rows on one work, and picking the lexically-first is the same
// determinism rule /search's tvdb_id projection takes — an arbitrary but
// STABLE answer beats one that moves with the physical row order.
func (a *API) externalIDs(ctx context.Context, entityType, entityID string) (map[string]string, error) {
	rows, err := a.reader.QueryContext(ctx,
		`SELECT source, value FROM external_ids
		 WHERE entity_type = ? AND entity_id = ? ORDER BY source, value`,
		entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var source, value string
		if err := rows.Scan(&source, &value); err != nil {
			return nil, err
		}
		if _, seen := out[source]; !seen {
			out[source] = value
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
	id := chi.URLParam(r, "id")
	row := a.reader.QueryRowContext(r.Context(),
		`SELECT `+editionColumns+` FROM editions WHERE id = ?`, id)
	edition, err := scanEdition(row)
	if err != nil {
		a.fail(w, r, "edition", err)
		return
	}
	ids, err := a.externalIDs(r.Context(), "edition", id)
	if err != nil {
		a.fail(w, r, "edition", err)
		return
	}
	a.write(w, r, http.StatusOK, EditionDetail{Edition: edition, ExternalIDs: ids})
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
	if frag, fargs := guestAssetFilter(r, "source_class"); frag != "" {
		where = append(where, frag)
		args = append(args, fargs...)
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
	// The guest content boundary (ADR-0074): a non-guest-visible asset does not
	// exist for a Guest — a 404, the same answer as an unknown id, so a Guest
	// cannot even confirm a hidden asset is there. Inert today (every asset is
	// `managed`); the vault boundary once personal content lands.
	if id, ok := httpapi.IdentityFrom(r.Context()); ok && id.Guest && !guest.Visible(asset.SourceClass) {
		httpapi.Fail(w, r, problem.NotFound("no asset is recorded with the id "+asset.ID))
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
	if frag, fargs := guestAssetFilter(r, "assets.source_class"); frag != "" {
		where = append(where, frag)
		args = append(args, fargs...)
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
	rows  interface{ Scan(...any) error }
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
