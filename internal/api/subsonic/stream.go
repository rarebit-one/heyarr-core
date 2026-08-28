package subsonic

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
)

// handleStream serves the bytes of one track, for both `stream` and `download`.
//
// # It delegates byte serving, and that is the point
//
// The adapter resolves a track id to a blob hash and a server-known MIME, then
// hands the response to the ordinary blob handler (ADR-0013). Range requests,
// 206 partial content and M10's progressive serving of a still-arriving blob
// are all inherited unchanged — the adapter adds no byte-path code of its own
// and stays as piece-agnostic as internal/api/blobs, exactly the discipline the
// webseed boundary guard exists to keep.
//
// # DIRECT only, deliberately
//
// Subsonic's stream endpoint can ask for transcoding (maxBitRate, format). This
// slice serves the original bytes and ignores those parameters: a music client
// decodes FLAC and MP3 itself, so DIRECT is the honest plan and driving the §68
// transcode planner here would be scope this milestone does not need yet. That
// is a documented boundary, not a silent one — a request for a format the bytes
// are not still receives the bytes as they are, which every Subsonic music
// client handles, rather than a fabricated transcode.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	p := parse(r)
	if _, code, msg := h.authenticate(r.Context(), p); code != 0 {
		h.write(w, p.format, h.fail(code, msg))
		return
	}

	editionID, ok := decodeTrackID(r.URL.Query().Get("id"))
	if !ok {
		h.write(w, p.format, h.fail(errNotFound, "track not found"))
		return
	}

	hash, mime, err := h.resolveTrack(r.Context(), editionID)
	if errors.Is(err, sql.ErrNoRows) {
		h.write(w, p.format, h.fail(errNotFound, "track not found"))
		return
	}
	if err != nil {
		h.internalError(w, r, p, "stream", err)
		return
	}
	if mime == "" {
		mime = blobs.OctetStream
	}

	// The blob handler reads its subject from the route, so the resolved hash
	// is placed there rather than passed — the same in-process handoff the
	// renderer route uses, past the point where authentication has already been
	// decided in Subsonic's own terms.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	inner := r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	h.blobs.ContentAs(w, inner, mime)
}

// resolveTrack maps a track (Edition) id to the blob it streams and the MIME to
// serve it as, choosing the same single asset per edition the listing did.
func (h *Handler) resolveTrack(ctx context.Context, editionID string) (hash, mime string, err error) {
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
