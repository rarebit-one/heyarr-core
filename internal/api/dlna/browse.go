package dlna

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/render"
)

// The read-only projection of the catalogue (works/editions/assets, §11) onto a
// UPnP ContentDirectory tree. Two levels, deliberately: the root lists a folder
// per content type, and a folder lists its playable items. The model is never
// reshaped to suit UPnP — a folder is a grouping of the content_type column and
// an item is one Asset — the same discipline the OpenSubsonic and OPDS adapters
// hold to (§70).
//
// Only assets whose bytes the render route can actually serve become items: a
// blob-backed asset (managed/vault) whose MIME is in render's servable table.
// Advertising an item a device would then fail to fetch is the dishonesty this
// filter refuses — the same reason a linked asset is never listed.

const (
	rootID   = "0"
	ctPrefix = "ct:"
	itemPref = "asset:"
)

// objectRow is one playable asset as the projection needs it.
type objectRow struct {
	contentType string
	assetID     string
	title       string
	year        sql.NullInt64
	mime        string
	size        int64
	blobHash    string
}

// browse answers a Browse action.
func (h *Handler) browse(w http.ResponseWriter, r *http.Request, req browseRequest) {
	switch {
	case req.BrowseFlag == "BrowseMetadata":
		h.browseMetadata(w, r, req.ObjectID)
	case req.ObjectID == rootID:
		h.browseRoot(w, r, req)
	case strings.HasPrefix(req.ObjectID, ctPrefix):
		h.browseContentType(w, r, req, strings.TrimPrefix(req.ObjectID, ctPrefix))
	default:
		// An unknown container is "no such object" (UPnP 701), not an empty
		// success — a control point distinguishes "nothing here" from "that is
		// not a thing".
		writeFault(w, 701, "no such object: "+req.ObjectID)
	}
}

func (h *Handler) browseRoot(w http.ResponseWriter, r *http.Request, req browseRequest) {
	counts, order, err := h.contentTypeCounts(r.Context())
	if err != nil {
		h.log.Error("dlna root browse failed", "error", err)
		writeFault(w, 501, "the server failed to answer that browse")
		return
	}
	d := newDIDL()
	for _, ct := range order {
		d.Containers = append(d.Containers, container{
			ID: ctPrefix + ct, ParentID: rootID, Restricted: 1,
			ChildCount: counts[ct], Title: folderTitle(ct), Class: classStorageFolder,
		})
	}
	total := len(order)
	d.Containers = page(d.Containers, req.StartingIndex, req.RequestedCount)
	h.respond(w, d, len(d.Containers), total)
}

func (h *Handler) browseContentType(w http.ResponseWriter, r *http.Request, req browseRequest, ct string) {
	rows, err := h.playableAssets(r.Context(), ct)
	if err != nil {
		h.log.Error("dlna content-type browse failed", "error", err, "content_type", ct)
		writeFault(w, 501, "the server failed to answer that browse")
		return
	}
	d := newDIDL()
	for _, row := range rows {
		it, ok := h.itemFor(row, ctPrefix+ct)
		if !ok {
			continue
		}
		d.Items = append(d.Items, it)
	}
	total := len(d.Items)
	d.Items = page(d.Items, req.StartingIndex, req.RequestedCount)
	h.respond(w, d, len(d.Items), total)
}

// browseMetadata returns the object's OWN description, which a control point
// asks for to render a header before it lists children.
func (h *Handler) browseMetadata(w http.ResponseWriter, r *http.Request, objectID string) {
	d := newDIDL()
	switch {
	case objectID == rootID:
		d.Containers = append(d.Containers, container{
			ID: rootID, ParentID: "-1", Restricted: 1, Title: h.friendlyName, Class: classStorageFolder,
		})
	case strings.HasPrefix(objectID, ctPrefix):
		ct := strings.TrimPrefix(objectID, ctPrefix)
		d.Containers = append(d.Containers, container{
			ID: objectID, ParentID: rootID, Restricted: 1, Title: folderTitle(ct), Class: classStorageFolder,
		})
	case strings.HasPrefix(objectID, itemPref):
		row, ok, err := h.assetByID(r.Context(), strings.TrimPrefix(objectID, itemPref))
		if err != nil {
			writeFault(w, 501, "the server failed to answer that browse")
			return
		}
		if !ok {
			writeFault(w, 701, "no such object: "+objectID)
			return
		}
		it, ok := h.itemFor(row, ctPrefix+row.contentType)
		if !ok {
			writeFault(w, 701, "no such object: "+objectID)
			return
		}
		d.Items = append(d.Items, it)
	default:
		writeFault(w, 701, "no such object: "+objectID)
		return
	}
	h.respond(w, d, 1, 1)
}

// itemFor builds the DIDL-Lite item for one asset, or reports that its bytes are
// not servable and it must be omitted.
func (h *Handler) itemFor(row objectRow, parentID string) (item, bool) {
	mime, ok := render.CanonicalMIME(row.mime)
	if !ok {
		return item{}, false
	}
	url, err := h.renderURL(row.blobHash, mime)
	if err != nil {
		return item{}, false
	}
	it := item{
		ID: itemPref + row.assetID, ParentID: parentID, Restricted: 1,
		Title: row.title, Class: classFor(row.contentType),
		Res: res{
			ProtocolInfo: "http-get:*:" + mime + ":*",
			Size:         row.size,
			URL:          url,
		},
	}
	if row.year.Valid && row.year.Int64 > 0 {
		it.Date = strconv.FormatInt(row.year.Int64, 10) + "-01-01"
	}
	return it, true
}

// respond marshals a DIDL-Lite document into a Browse response.
func (h *Handler) respond(w http.ResponseWriter, d *didlLite, returned, total int) {
	result, err := d.marshal()
	if err != nil {
		writeFault(w, 501, "the server failed to render that browse")
		return
	}
	writeBrowseResponse(w, browseResponse{
		Result: result, NumberReturned: returned, TotalMatches: total, UpdateID: 0,
	})
}

// contentTypeCounts groups the servable assets by content type. The servability
// filter runs in Go because it is render's table, not the database's, that
// decides it; a production instance with a large catalogue would push a
// mime-set predicate into SQL, and this is noted rather than hidden.
func (h *Handler) contentTypeCounts(ctx context.Context) (map[string]int, []string, error) {
	rows, err := h.reader.QueryContext(ctx, `
		SELECT w.content_type, COALESCE(a.mime, b.mime, '')
		FROM assets a
		JOIN blobs b ON b.hash = a.blob_hash
		JOIN editions e ON e.id = a.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE a.blob_hash IS NOT NULL
		ORDER BY w.content_type`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	var order []string
	for rows.Next() {
		var ct, mime string
		if err := rows.Scan(&ct, &mime); err != nil {
			return nil, nil, err
		}
		if _, ok := render.CanonicalMIME(mime); !ok {
			continue
		}
		if _, seen := counts[ct]; !seen {
			order = append(order, ct)
		}
		counts[ct]++
	}
	return counts, order, rows.Err()
}

// playableAssets returns one row per blob-backed asset of a content type, in a
// stable order so a control point can page it.
func (h *Handler) playableAssets(ctx context.Context, contentType string) ([]objectRow, error) {
	rows, err := h.reader.QueryContext(ctx, `
		SELECT w.content_type, a.id, w.title, w.year,
		       COALESCE(a.mime, b.mime, ''), b.size, a.blob_hash
		FROM assets a
		JOIN blobs b ON b.hash = a.blob_hash
		JOIN editions e ON e.id = a.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE a.blob_hash IS NOT NULL AND w.content_type = ?
		ORDER BY w.sort_title, a.id`, contentType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObjectRows(rows)
}

// assetByID resolves one asset for BrowseMetadata.
func (h *Handler) assetByID(ctx context.Context, assetID string) (objectRow, bool, error) {
	rows, err := h.reader.QueryContext(ctx, `
		SELECT w.content_type, a.id, w.title, w.year,
		       COALESCE(a.mime, b.mime, ''), b.size, a.blob_hash
		FROM assets a
		JOIN blobs b ON b.hash = a.blob_hash
		JOIN editions e ON e.id = a.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE a.blob_hash IS NOT NULL AND a.id = ?`, assetID)
	if err != nil {
		return objectRow{}, false, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanObjectRows(rows)
	if err != nil || len(out) == 0 {
		return objectRow{}, false, err
	}
	return out[0], true, nil
}

func scanObjectRows(rows *sql.Rows) ([]objectRow, error) {
	var out []objectRow
	for rows.Next() {
		var row objectRow
		if err := rows.Scan(&row.contentType, &row.assetID, &row.title, &row.year,
			&row.mime, &row.size, &row.blobHash); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// page applies a UPnP StartingIndex/RequestedCount window. RequestedCount 0
// means "all from the start index", which is what a control point sends to pull
// a whole small folder in one request.
func page[T any](items []T, start, count int) []T {
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []T{}
	}
	items = items[start:]
	if count > 0 && count < len(items) {
		items = items[:count]
	}
	return items
}

func folderTitle(contentType string) string {
	switch contentType {
	case "movie":
		return "Movies"
	case "series":
		return "TV"
	case "music":
		return "Music"
	default:
		if contentType == "" {
			return "Other"
		}
		return strings.ToUpper(contentType[:1]) + contentType[1:]
	}
}
