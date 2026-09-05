package resources

import (
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
func artworkPick(r *http.Request, workRef string) pick {
	return assetPick(r, workRef, "artwork", artworkRank)
}

// primaryPick selects the one file a card tap should play: the first
// `primary`-role asset of the work, in creation order. A work with several
// editions has several; the first is an arbitrary but STABLE answer, and a
// client that wants to choose lists the work's assets.
func primaryPick(r *http.Request, workRef string) pick {
	return assetPick(r, workRef, "primary", "")
}

func assetPick(r *http.Request, workRef, role, rank string) pick {
	where := []string{
		"e2.work_id = " + workRef,
		"a2.role = ?",
		"a2.blob_hash IS NOT NULL",
		"a2.missing_since IS NULL",
	}
	args := []any{role}
	if frag, fargs := guestAssetFilter(r, "a2.source_class"); frag != "" {
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
func cardQuery(r *http.Request, withArtwork, withPrimary bool) (string, []any) {
	cols := prefixed(workColumns, "works") + ", works.created_at"
	from := ` FROM works`
	var args []any
	if withArtwork {
		p := artworkPick(r, "works.id")
		cols += ", " + artworkColumns
		from += ` LEFT JOIN assets art ON art.id = ` + p.sql
		args = append(args, p.args...)
	} else {
		cols += ", " + nullArtwork
	}
	if withPrimary {
		p := primaryPick(r, "works.id")
		cols += ", " + primaryColumns
		from += ` LEFT JOIN assets pa ON pa.id = ` + p.sql +
			` LEFT JOIN blobs pb ON pb.hash = pa.blob_hash` +
			` LEFT JOIN blob_probes pp ON pp.blob_hash = pa.blob_hash`
		args = append(args, p.args...)
	} else {
		cols += ", " + nullPrimary
	}
	return `SELECT ` + cols + from, args
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
	p := artworkPick(r, "works.id")
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
