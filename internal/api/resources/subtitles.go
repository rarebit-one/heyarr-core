package resources

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// SubtitleBackfillRequest is the POST /subtitles/backfill body.
//
// Exactly one scope is required: a library, a work, or an explicit all. The
// scope is mandatory rather than defaulting to everything because backfilling a
// whole node is a large batch of jobs, and "I meant this one show" should not be
// one forgotten flag away from "every video you have".
type SubtitleBackfillRequest struct {
	LibraryID string `json:"library_id"`
	WorkID    string `json:"work_id"`
	All       bool   `json:"all"`
}

// backfillSubtitles enqueues an extract_subtitles job for every managed video in
// scope whose Edition has no subtitle yet (ADR-0084).
//
// It exists because extraction runs at INGEST, like the probe and the remux — a
// video ingested before ADR-0084, or before this node had ffmpeg, has embedded
// tracks that were never lifted out, and a rescan will not fix it (the bytes are
// unchanged, so the ingest deduplicates and never re-enqueues). This is the
// operator's lever to reprocess that existing content once, on demand.
//
// "No subtitle yet" is the filter on purpose: a title that already has a
// sidecar (an external .srt, or a track extracted on a previous run) is left
// alone, so a re-run is cheap and does not pile a second English caption onto an
// Edition that already has one. The extract job is itself a no-op on a video
// with no embedded TEXT tracks, so a false positive costs a subprocess, not a
// bad asset.
func (a *API) backfillSubtitles(w http.ResponseWriter, r *http.Request) {
	var body SubtitleBackfillRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if body.LibraryID == "" && body.WorkID == "" && !body.All {
		httpapi.Fail(w, r, problem.BadRequest(
			"a scope is required: library_id, work_id, or all=true — refusing to guess between one show and the whole node"))
		return
	}

	videos, err := a.videosNeedingSubtitleExtraction(r.Context(), body.LibraryID, body.WorkID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}

	enqueued := 0
	for _, v := range videos {
		if _, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
			Type:               ffmpeg.ExtractJobType,
			Payload:            ffmpeg.ExtractPayload{BlobHash: v.blobHash, AssetID: v.assetID},
			DedupeKey:          ffmpeg.ExtractDedupeKey(v.blobHash),
			RequiredCapability: ffmpeg.Capability,
		}); err != nil {
			a.fail(w, r, "job", err)
			return
		}
		enqueued++
	}

	a.write(w, r, http.StatusAccepted, map[string]any{
		"candidates": len(videos),
		"enqueued":   enqueued,
	})
}

type videoRef struct {
	assetID  string
	blobHash string
}

// videosNeedingSubtitleExtraction finds managed video assets whose Edition holds
// no subtitle asset, within an optional library and/or work scope. Empty scope
// args mean "no filter on that dimension" — the handler has already refused a
// wholly-unscoped request unless `all` was set.
func (a *API) videosNeedingSubtitleExtraction(ctx context.Context, libraryID, workID string) ([]videoRef, error) {
	where := []string{
		"a.source_class = 'managed'",
		"a.blob_hash IS NOT NULL",
		"a.mime LIKE 'video/%'",
		// Not already captioned: no subtitle asset on the same Edition.
		"NOT EXISTS (SELECT 1 FROM assets s WHERE s.edition_id = a.edition_id AND s.role = 'subtitle')",
	}
	var args []any
	if libraryID != "" {
		where = append(where, "a.library_id = ?")
		args = append(args, libraryID)
	}
	if workID != "" {
		where = append(where, "e.work_id = ?")
		args = append(args, workID)
	}

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `
		SELECT a.id, a.blob_hash
		FROM assets a
		JOIN editions e ON e.id = a.edition_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.filename`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []videoRef
	for rows.Next() {
		var id, blob sql.NullString
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		if id.Valid && blob.Valid {
			out = append(out, videoRef{assetID: id.String, blobHash: blob.String})
		}
	}
	return out, rows.Err()
}
