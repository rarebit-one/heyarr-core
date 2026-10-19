package opds

import (
	"net/http"
	"time"
)

// mimeByFormat maps a Heyarr edition format to the media type an OPDS reader
// dispatches on. It is only a fallback: the asset's stored MIME is the
// authority when present, and this fills in for an asset ingested without one.
var mimeByFormat = map[string]string{
	"epub": "application/epub+zip",
	"pdf":  "application/pdf",
	"cbz":  "application/x-cbz",
	"cbr":  "application/x-cbr",
	"cb7":  "application/x-cb7",
	"cbt":  "application/x-cbt",
	"mobi": "application/x-mobipocket-ebook",
	"azw":  "application/vnd.amazon.ebook",
	"azw3": "application/vnd.amazon.ebook",
	"djvu": "image/vnd.djvu",
	"fb2":  "application/x-fictionbook+xml",
	"pdb":  "application/vnd.palm",
	"lit":  "application/x-ms-reader",
	"html": "text/html",
	"htm":  "text/html",
}

const octetStream = "application/octet-stream"

// acquisitionType picks the media type for an acquisition link: the stored MIME
// if the ingest recorded one, else the format map, else a generic fallback so a
// download is still offered rather than hidden.
func acquisitionMediaType(storedMIME, format string) string {
	if storedMIME != "" {
		return storedMIME
	}
	if m, ok := mimeByFormat[format]; ok {
		return m
	}
	return octetStream
}

// handlePublications is the acquisition feed: every book or document with at
// least one streamable format, one Atom entry each, an acquisition link per
// primary Asset. Several files can share an Edition and still get separate links.
//
// A publication is a book or document Work; a format is an Edition, and each
// downloadable file is an Asset. A captured article is book-like — a titled Work whose bytes a
// reader downloads — so it surfaces through the same shelf the navigation feed
// already promises ("every book, comic and document"), rather than a parallel
// feed. Only assets with a managed or vault blob are offered — a linked asset
// has no blob (ADR-0020) and nothing to download, so advertising it would be an
// acquisition link that 404s. A work whose only formats are linked has no entry
// at all rather than an entry a reader cannot acquire from.
func (h *Handler) handlePublications(w http.ResponseWriter, r *http.Request) {
	rows, err := h.reader.QueryContext(r.Context(), `
		SELECT w.id, w.title, COALESCE(json_extract(w.attributes, '$.author'), ''),
		       e.id, a.id, e.edition_type, COALESCE(a.mime, b.mime, ''), COALESCE(a.filename, '')
		FROM works w
		JOIN editions e ON e.work_id = w.id
		JOIN assets a ON a.edition_id = e.id AND a.blob_hash IS NOT NULL AND a.role = 'primary'
		JOIN blobs b ON b.hash = a.blob_hash
		WHERE w.content_type IN ('book', 'document')
		ORDER BY w.sort_title, w.id, e.edition_type, e.id, a.id
		LIMIT 5000`)
	if err != nil {
		h.internalError(w, "publications", err)
		return
	}
	defer func() { _ = rows.Close() }()

	stamp := h.now().UTC().Format(time.RFC3339)
	var entries []entry
	var cur *entry
	var curWork string
	var curEdition string
	for rows.Next() {
		var workID, title, authorName, editionID, assetID, format, mime, filename string
		if err := rows.Scan(&workID, &title, &authorName, &editionID, &assetID, &format, &mime, &filename); err != nil {
			h.internalError(w, "publications", err)
			return
		}
		if cur == nil || curWork != workID {
			entries = append(entries, entry{
				Title:   title,
				ID:      "urn:heyarr:work:" + workID,
				Updated: stamp,
			})
			cur = &entries[len(entries)-1]
			curWork = workID
			if authorName != "" {
				cur.Authors = []author{{Name: authorName}}
			}
		}
		// Keep the first asset at its legacy edition URL. Each later file
		// gets an asset URL, so old links remain valid and no volume vanishes.
		downloadID := editionID
		var linkTitle string
		if editionID == curEdition {
			downloadID = assetID
			linkTitle = filename
		}
		curEdition = editionID
		cur.Links = append(cur.Links, link{
			Rel:   relAcquisition,
			Href:  Prefix + "/download/" + downloadID,
			Type:  acquisitionMediaType(mime, format),
			Title: linkTitle,
		})
	}
	if err := rows.Err(); err != nil {
		h.internalError(w, "publications", err)
		return
	}

	writeFeed(w, acquisitionType, feed{
		ID:      "urn:heyarr:opds:publications",
		Title:   "All Publications",
		Updated: stamp,
		Links: []link{
			{Rel: relSelf, Href: Prefix + "/publications", Type: acquisitionType},
			{Rel: relStart, Href: Prefix, Type: navType},
		},
		Entries: entries,
	})
}

// internalError logs a query failure and returns a 500, never the underlying
// message — a reader cannot act on a SQL error and should not see the schema.
func (h *Handler) internalError(w http.ResponseWriter, op string, err error) {
	h.log.Error("opds query failed", "op", op, "error", err)
	http.Error(w, "the server failed to answer that request", http.StatusInternalServerError)
}
