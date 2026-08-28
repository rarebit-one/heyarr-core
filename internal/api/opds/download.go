package opds

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
)

// handleDownload serves the bytes of one publication format.
//
// It resolves the edition id in the path to a blob hash and a server-known
// MIME, then hands the response to the ordinary blob handler (ADR-0013): Range
// requests and 206 partial content are inherited unchanged, and the adapter
// adds no byte-path code of its own — the same in-process delegation the
// renderer and OpenSubsonic routes use, past the point where Basic auth has
// already decided the caller may read.
func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	editionID := chi.URLParam(r, "id")

	hash, mime, err := h.resolve(r.Context(), editionID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such publication", http.StatusNotFound)
		return
	}
	if err != nil {
		h.internalError(w, "download", err)
		return
	}
	if mime == "" {
		mime = blobs.OctetStream
	}

	// The blob handler reads its subject from the route, so the resolved hash is
	// placed there rather than passed.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	inner := r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	h.blobs.ContentAs(w, inner, mime)
}

// resolve maps an edition id to the blob it serves and the MIME to serve it as,
// choosing the same single streamable asset per edition the feed did.
func (h *Handler) resolve(ctx context.Context, editionID string) (hash, mime string, err error) {
	err = h.reader.QueryRowContext(ctx, `
		SELECT b.hash, COALESCE(a.mime, b.mime, '')
		FROM editions e
		JOIN assets a ON a.id = (
			SELECT a2.id FROM assets a2
			WHERE a2.edition_id = e.id AND a2.blob_hash IS NOT NULL
			ORDER BY a2.id LIMIT 1)
		JOIN blobs b ON b.hash = a.blob_hash
		WHERE e.id = ?`, editionID).Scan(&hash, &mime)
	return hash, mime, err
}
