package resources

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// Browse is a projection over the catalog (ADR-0075).
//
// A consumer client — a phone, a television — wants the catalog the way a
// shelf presents it: a poster, the one file to play when the card is tapped,
// the newest things first. None of that is a new entity. A poster is the
// `artwork`-role asset a scan already recorded; the playable file is the
// `primary`-role asset; "recently added" is `created_at`. This file is where
// those projections are derived, once, so the list, the detail read, the
// artwork redirect and (later) search and the grouping reads pick the same
// asset for the same work.

// pick is a correlated subquery selecting ONE asset id for a work, with the
// bound values it needs. It is spliced into a LEFT JOIN so a work with no
// such asset still lists — as a card with a null poster, never as a missing
// row.
type pick struct {
	sql  string
	args []any
}

// artworkRank orders a work's artwork assets so that the front-facing image
// wins: a poster or cover before an unnamed image, an unnamed image before
// fanart, backdrops and banners (which are decoration, not identity). The id
// breaks ties, so the answer is stable across reads (ADR-0017).
const artworkRank = `CASE
	WHEN lower(a2.filename) GLOB 'poster*' OR lower(a2.filename) GLOB 'cover*'
	  OR lower(a2.filename) GLOB 'folder*' OR lower(a2.filename) GLOB 'front*' THEN 0
	WHEN lower(a2.filename) GLOB 'fanart*' OR lower(a2.filename) GLOB 'backdrop*'
	  OR lower(a2.filename) GLOB 'banner*' THEN 2
	ELSE 1 END`

// artworkPick selects the representative artwork asset of the work whose id
// is at workRef (a column reference, e.g. `works.id`).
//
// Only an asset that HOLDS bytes qualifies: a `linked` artwork has no blob
// (ADR-0020) and nothing serves it, and an asset gone missing from disk is
// not a poster a client can fetch. The guest boundary (ADR-0074) applies here
// exactly as it does on the asset listings — a Guest never learns a vault
// poster exists, not even as a redirect target.
func artworkPick(ctx context.Context, workRef string) pick {
	return assetPick(ctx, "e2.work_id = "+workRef, "artwork", artworkRank)
}

// primaryPick selects the one file a card tap should play: the first
// `primary`-role asset of the work, in creation order. A work with several
// editions has several; the first is an arbitrary but STABLE answer, and a
// client that wants to choose lists the work's assets.
func primaryPick(ctx context.Context, workRef string) pick {
	return assetPick(ctx, "e2.work_id = "+workRef, "primary", "")
}

// editionPrimaryPick is primaryPick scoped to ONE edition (editionRef is a
// column reference, e.g. `e.id`) — an episode's file, for a search hit.
func editionPrimaryPick(ctx context.Context, editionRef string) pick {
	return assetPick(ctx, "a2.edition_id = "+editionRef, "primary", "")
}

// assetPick is the shared correlated subquery: scope is the literal predicate
// that ties the asset to its owner (a work through its editions, or an edition
// directly); role and rank choose and order among the candidates.
func assetPick(ctx context.Context, scope, role, rank string) pick {
	where := []string{
		scope,
		"a2.role = ?",
		"a2.blob_hash IS NOT NULL",
		"a2.missing_since IS NULL",
	}
	args := []any{role}
	if frag, fargs := guestAssetFilterCtx(ctx, "a2.source_class"); frag != "" {
		where = append(where, frag)
		args = append(args, fargs...)
	}
	order := "a2.id"
	if rank != "" {
		order = rank + ", a2.id"
	}
	return pick{
		sql: `(SELECT a2.id FROM assets a2 JOIN editions e2 ON e2.id = a2.edition_id
			WHERE ` + strings.Join(where, " AND ") + `
			ORDER BY ` + order + ` LIMIT 1)`,
		args: args,
	}
}

// cardJoins are the LEFT JOINs that fetch a work's artwork and primary asset
// beside the work row, and cardColumns the columns they contribute — in the
// order scanCard reads them. Kept together so the two cannot drift.
//
// When an embed is not requested the join is replaced by NULL literals of the
// same arity: the scanner then has one shape, and a listing that asked for
// nothing extra costs nothing extra.
const (
	artworkColumns = `art.id, art.blob_hash, art.mime`
	primaryColumns = `pa.id, pa.edition_id, pa.blob_hash, COALESCE(pa.mime, pb.mime), pb.size, pp.duration_seconds`
	nullArtwork    = `NULL, NULL, NULL`
	nullPrimary    = `NULL, NULL, NULL, NULL, NULL, NULL`
)

// cardQuery assembles the SELECT/FROM half of a card read over `works`.
//
// The returned args are the picker bindings, which come BEFORE any WHERE
// bindings the caller appends: SQLite binds positionally in textual order,
// and the pickers live in the join clauses.
func cardQuery(ctx context.Context, withArtwork, withPrimary bool) (string, []any) {
	cols := prefixed(workColumns, "works") + ", works.created_at"
	from := ` FROM works`
	var args []any
	if withArtwork {
		p := artworkPick(ctx, "works.id")
		cols += ", " + artworkColumns
		from += ` LEFT JOIN assets art ON art.id = ` + p.sql
		args = append(args, p.args...)
	} else {
		cols += ", " + nullArtwork
	}
	if withPrimary {
		p := primaryPick(ctx, "works.id")
		cols += ", " + primaryColumns
		from += primaryJoins(p)
		args = append(args, p.args...)
	} else {
		cols += ", " + nullPrimary
	}
	return `SELECT ` + cols + from, args
}

// primaryJoins are the three LEFT JOINs that resolve a picked primary asset
// (alias pa) with its blob (pb) and probe (pp). Shared with the search reads.
func primaryJoins(p pick) string {
	return ` LEFT JOIN assets pa ON pa.id = ` + p.sql +
		` LEFT JOIN blobs pb ON pb.hash = pa.blob_hash` +
		` LEFT JOIN blob_probes pp ON pp.blob_hash = pa.blob_hash`
}

// scanPrimary reads the primaryColumns into a ref, or nil when the pick found
// nothing. dest is the scanner's six destinations, in primaryColumns order.
type primaryScan struct {
	id, edition, blob, mime sql.NullString
	size                    sql.NullInt64
	duration                sql.NullFloat64
}

func (p *primaryScan) dests() []any {
	return []any{&p.id, &p.edition, &p.blob, &p.mime, &p.size, &p.duration}
}

func (p *primaryScan) ref() *PrimaryAssetRef {
	if !p.id.Valid || !p.blob.Valid {
		return nil
	}
	out := &PrimaryAssetRef{
		AssetID: p.id.String, EditionID: p.edition.String, BlobHash: p.blob.String,
		MIME: nullString(p.mime), Size: nullInt(p.size), ContentURL: blobContentURL(p.blob.String),
	}
	if p.duration.Valid {
		d := p.duration.Float64
		out.DurationSeconds = &d
	}
	return out
}

// artworkScan is scanPrimary's sibling for artworkColumns.
type artworkScan struct{ id, blob, mime sql.NullString }

func (a *artworkScan) dests() []any { return []any{&a.id, &a.blob, &a.mime} }

func (a *artworkScan) ref() *ArtworkRef {
	if !a.id.Valid || !a.blob.Valid {
		return nil
	}
	return &ArtworkRef{AssetID: a.id.String, BlobHash: a.blob.String, MIME: nullString(a.mime), ContentURL: blobContentURL(a.blob.String)}
}

// cardRow is one scanned card: the work, the raw created_at the recent-first
// cursor keys on (re-rendering a parsed time can differ from the stored text,
// and a keyset boundary must be the stored text), and the two embeds.
type cardRow struct {
	Work
	createdRaw string
	artwork    *ArtworkRef
	primary    *PrimaryAssetRef
}

func scanCard(rows interface{ Scan(...any) error }) (cardRow, error) {
	var c cardRow
	var artID, artBlob, artMIME sql.NullString
	var paID, paEdition, paBlob, paMIME sql.NullString
	var paSize sql.NullInt64
	var paDuration sql.NullFloat64
	work, err := scanWork(appendedScan{rows: rows, extra: []any{
		&c.createdRaw,
		&artID, &artBlob, &artMIME,
		&paID, &paEdition, &paBlob, &paMIME, &paSize, &paDuration,
	}})
	if err != nil {
		return cardRow{}, err
	}
	c.Work = work
	if artID.Valid && artBlob.Valid {
		c.artwork = &ArtworkRef{
			AssetID:    artID.String,
			BlobHash:   artBlob.String,
			MIME:       nullString(artMIME),
			ContentURL: blobContentURL(artBlob.String),
		}
	}
	if paID.Valid && paBlob.Valid {
		c.primary = &PrimaryAssetRef{
			AssetID:    paID.String,
			EditionID:  paEdition.String,
			BlobHash:   paBlob.String,
			MIME:       nullString(paMIME),
			Size:       nullInt(paSize),
			ContentURL: blobContentURL(paBlob.String),
		}
		if paDuration.Valid {
			d := paDuration.Float64
			c.primary.DurationSeconds = &d
		}
	}
	return c, nil
}

// blobContentURL is the ordinary byte route for a blob (ADR-0013) — the same
// URL StartPlaybackResponse.ContentURL hands out, so a client has one way to
// fetch bytes whatever read pointed it at them.
func blobContentURL(hash string) string {
	return httpapi.APIPrefix + "/blobs/" + hash + "/content"
}

// getWorkArtwork answers a work's poster by REDIRECTING to its blob.
//
// It is a redirect and not a second byte route on purpose. The blob route is
// the one contract for bytes (ADR-0013), and its cache headers — immutable, a
// year — are correct only because the URL is the hash. A work's poster can
// change (a rescan finds a better one), so a URL keyed by the work must not
// carry those headers; a short private max-age here and the immutable ones on
// the target give a client both. 307 rather than 302 so a HEAD stays a HEAD.
//
// There is no `?size=`. The node has no image library and no thumbnail cache;
// a client sizes the poster it fetches. ADR-0075 records the revisit trigger.
func (a *API) getWorkArtwork(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p := artworkPick(r.Context(), "works.id")
	args := append(p.args, id)
	var blob sql.NullString
	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	err := a.reader.QueryRowContext(r.Context(),
		`SELECT art.blob_hash FROM works LEFT JOIN assets art ON art.id = `+p.sql+
			` WHERE works.id = ?`, args...).Scan(&blob)
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	if !blob.Valid {
		// Distinct from the unknown-work 404 in its detail: "add a poster"
		// and "you asked for the wrong thing" are different answers.
		httpapi.Fail(w, r, problem.NotFound("the work has no artwork"))
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Redirect(w, r, blobContentURL(blob.String), http.StatusTemporaryRedirect)
}

// parseInclude reads a comma-separated `include` list against a closed set.
// An unknown name is a 400 for the same reason an unknown filter value is:
// silently ignoring `include=artwrok` hands a client a page with no posters
// and no explanation.
func parseInclude(r *http.Request, allowed ...string) (map[string]bool, error) {
	out := map[string]bool{}
	raw := strings.TrimSpace(r.URL.Query().Get("include"))
	if raw == "" {
		return out, nil
	}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if name == a {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("include must name only %s, not %q", strings.Join(allowed, ", "), name)
		}
		out[name] = true
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Groupings: artists, authors (ADR-0075)
// ---------------------------------------------------------------------------

// GroupSummary is one artist (or author): a grouping over works keyed by the
// name the identifier wrote into `attributes`, not an entity. The albums (or
// books) are `GET /works?content_type=…&artist=<name>` (or `author=`).
type GroupSummary struct {
	Name      string `json:"name"`
	WorkCount int64  `json:"work_count"`
	// Artwork is the poster of the group's first work (by year, then title) —
	// an album cover standing in for the artist, a book cover for the author —
	// or null when none of them has one the caller may see.
	Artwork *ArtworkRef `json:"artwork"`
}

// listGrouped pages the distinct non-empty values of one attribute across the
// works of one content type, with a count and a representative poster.
//
// Keyset on the lower-cased name (arity 1, under its own collection so a
// works cursor is refused). `q` is a substring filter on the name. The
// grouping is a CTE so the representative-artwork pick runs once per GROUP,
// not once per work.
func (a *API) listGrouped(attr, contentType, collection string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := parseQuery(r, collection, 1)
		if err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
		ctx := r.Context()
		// The representative work: the group's first by year then title, whose
		// poster stands in for the group. Its id feeds the ordinary artwork pick.
		rep := `(SELECT w2.id FROM works w2 WHERE w2.content_type = ?
			AND json_extract(w2.attributes, '$.` + attr + `') = g.name
			ORDER BY w2.year, w2.sort_title, w2.id LIMIT 1)`
		art := artworkPick(ctx, rep)

		where := []string{"w.content_type = ?", "name IS NOT NULL", "trim(name) <> ''"}
		args := []any{contentType}
		if term := r.URL.Query().Get("q"); term != "" {
			where = append(where, `lower(name) LIKE ? ESCAPE '\'`)
			args = append(args, strings.ToLower(likePattern(term)))
		}
		having := "1 = 1"
		if q.cursor != nil {
			having = "lower(name) > ?"
			args = append(args, q.cursor[0])
		}
		// Bind order follows textual order: the CTE's WHERE/HAVING, then the
		// representative subquery's content type, then the artwork pick.
		args = append(args, contentType)
		args = append(args, art.args...)
		args = append(args, q.limit+1)

		//nolint:gosec // attr is one of two package literals; every value is bound
		stmt := `WITH g AS (
				SELECT json_extract(w.attributes, '$.` + attr + `') AS name, COUNT(*) AS n
				FROM works w
				WHERE ` + strings.Join(where, " AND ") + `
				GROUP BY name HAVING ` + having + `
			)
			SELECT g.name, g.n, ` + artworkColumns + `
			FROM g LEFT JOIN assets art ON art.id = ` + art.sql + `
			ORDER BY lower(g.name), g.name LIMIT ?`

		rows, err := a.reader.QueryContext(ctx, stmt, args...)
		if err != nil {
			a.fail(w, r, collection, err)
			return
		}
		defer func() { _ = rows.Close() }()

		var groups []GroupSummary
		for rows.Next() {
			var g GroupSummary
			var as artworkScan
			if err := rows.Scan(append([]any{&g.Name, &g.WorkCount}, as.dests()...)...); err != nil {
				a.fail(w, r, collection, err)
				return
			}
			g.Artwork = as.ref()
			groups = append(groups, g)
		}
		if err := rows.Err(); err != nil {
			a.fail(w, r, collection, err)
			return
		}
		a.write(w, r, http.StatusOK, newPage(groups, q.limit,
			func(g GroupSummary) []string { return []string{strings.ToLower(g.Name)} }, collection))
	}
}
