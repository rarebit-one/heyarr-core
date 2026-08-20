package resources

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// Publications: EPUB, PDF, CBZ and CBR, stored and served, never rendered
// (§69).
//
// # There is no publication byte route, and that is deliberate
//
// A reader fetches bytes from GET /api/v1/blobs/{hash}/content like everything
// else. ADR-0013 makes that one endpoint with four consumers, and a fifth
// consumer that wants its own is a fifth consumer that has misunderstood the
// contract: byte ranges over an immutable object are byte ranges over an
// immutable object, whether the client is a video player, a replicator, a
// BitTorrent web-seed or an EPUB reader.
//
// What this file adds is the CATALOG view — enough for a client to tell an
// EPUB from a CBZ, and how long it is, without downloading it first.
//
// # Heyarr manages metadata; it does not invent it
//
// Counts come from the container's own manifest and are absent when it does
// not publish one. PDF and CBR report no count at all, because reading them
// needs a PDF parser and a RAR decoder — dependencies taken solely to produce
// a number, and both uncomfortably close to the line §69 draws. That cost is
// visible rather than hidden, and internal/domain/publication says so at
// length.

// Publication is the wire type: an asset that is one of §69's four containers,
// with what its container declared.
type Publication struct {
	AssetID   string `json:"asset_id"`
	EditionID string `json:"edition_id"`
	WorkID    string `json:"work_id"`
	Title     string `json:"title"`
	BlobHash  string `json:"blob_hash"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Format    string `json:"format"`
	// PageCount and ChapterCount are absent when unread. Absent is not zero: an
	// empty comic is a zero-page comic, and a PDF is a publication whose length
	// Heyarr has not counted. A client rendering "0 pages" for the second has
	// been told something false.
	PageCount    *int64 `json:"page_count,omitempty"`
	ChapterCount *int64 `json:"chapter_count,omitempty"`
	// ContentURL is where the bytes are. It is the ordinary blob endpoint,
	// named here so a client does not have to assemble it and so that the
	// single-endpoint contract is visible in the response rather than only in
	// an ADR.
	ContentURL string `json:"content_url"`
}

const publicationColumns = `a.id, a.edition_id, e.work_id, w.title, a.blob_hash,
	COALESCE(a.filename, ''), b.size, p.format, p.page_count, p.chapter_count`

const publicationFrom = `FROM publications p
	JOIN blobs b ON b.hash = p.blob_hash
	JOIN assets a ON a.blob_hash = p.blob_hash
	JOIN editions e ON e.id = a.edition_id
	JOIN works w ON w.id = e.work_id`

func scanPublicationRow(row interface{ Scan(...any) error }) (Publication, error) {
	var p Publication
	var blobHash sql.NullString
	var pages, chapters sql.NullInt64
	if err := row.Scan(&p.AssetID, &p.EditionID, &p.WorkID, &p.Title, &blobHash,
		&p.Filename, &p.Size, &p.Format, &pages, &chapters); err != nil {
		return Publication{}, err
	}
	p.BlobHash = blobHash.String
	if pages.Valid {
		p.PageCount = &pages.Int64
	}
	if chapters.Valid {
		p.ChapterCount = &chapters.Int64
	}
	p.ContentURL = httpapi.APIPrefix + "/blobs/" + p.BlobHash + "/content"
	return p, nil
}

func (a *API) listPublications(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r, "publications", 1)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	format, err := oneOf(r, "format", "epub", "pdf", "cbz", "cbr")
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if format != "" {
		where = append(where, "p.format = ?")
		args = append(args, format)
	}
	if q.cursor != nil {
		where = append(where, "a.id > ?")
		args = append(args, q.cursor[0])
	}
	args = append(args, q.limit+1)

	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT ` + publicationColumns + ` ` + publicationFrom +
		` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY a.id ASC LIMIT ?`

	rows, err := a.reader.QueryContext(r.Context(), stmt, args...)
	if err != nil {
		a.fail(w, r, "publication", err)
		return
	}
	defer func() { _ = rows.Close() }()

	var items []Publication
	for rows.Next() {
		p, err := scanPublicationRow(rows)
		if err != nil {
			a.fail(w, r, "publication", err)
			return
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "publication", err)
		return
	}
	a.write(w, r, http.StatusOK, newPage(items, q.limit,
		func(p Publication) []string { return []string{p.AssetID} }, "publications"))
}

// getPublication is keyed by ASSET id, not blob hash.
//
// The metadata is stored against the blob because it describes the bytes, but
// a client holds an asset: one blob can be two assets with two filenames in two
// libraries, and "which publication" is a question about a use of the bytes,
// not about the bytes.
func (a *API) getPublication(w http.ResponseWriter, r *http.Request) {
	p, err := scanPublicationRow(a.reader.QueryRowContext(r.Context(),
		`SELECT `+publicationColumns+` `+publicationFrom+` WHERE a.id = ?`,
		chi.URLParam(r, "id")))
	if err != nil {
		a.fail(w, r, "publication", err)
		return
	}
	a.write(w, r, http.StatusOK, p)
}
