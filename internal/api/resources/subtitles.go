package resources

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
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

// SubtitleWantRequest is the POST /subtitles/want body.
//
// Like the backfill door, exactly one scope is required and refused otherwise —
// wanting subtitles for the whole node is a large batch of wants and must not be
// one forgotten flag from "this one show". Unlike backfill, a language is
// required: a subtitle want is FOR a language (ADR-0085), and there is no honest
// default to fetch.
type SubtitleWantRequest struct {
	LibraryID string `json:"library_id"`
	WorkID    string `json:"work_id"`
	All       bool   `json:"all"`
	Language  string `json:"language"`
}

// requestSubtitles creates a subtitle want for every managed video in scope that
// holds no subtitle of the requested language (ADR-0085). The fetch driver
// (#505) then acquires each from a provider.
//
// It is the door for a subtitle that exists NOWHERE — neither shipped, embedded
// (ADR-0084's backfill covers those) nor already fetched. It is the fetch
// counterpart of the extract backfill: same scoping, but it creates wants rather
// than extract jobs, because these bytes are not in the file and must be found on
// a provider network.
//
// Idempotent, for the same reason backfill is cheap on a re-run: a video that
// already holds a subtitle of the language is skipped by the scan, and a want
// that already exists surfaces the uniqueness violation, which is counted as
// "already wanted" rather than failed (IsDuplicateWant). So an operator can run
// it repeatedly and only the genuinely-missing get a new want.
func (a *API) requestSubtitles(w http.ResponseWriter, r *http.Request) {
	var body SubtitleWantRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	lang := strings.ToLower(strings.TrimSpace(body.Language))
	if lang == "" {
		httpapi.Fail(w, r, problem.BadRequest(
			"a language is required: a subtitle want is for a language (e.g. \"en\"), and there is no default to fetch"))
		return
	}
	if body.LibraryID == "" && body.WorkID == "" && !body.All {
		httpapi.Fail(w, r, problem.BadRequest(
			"a scope is required: library_id, work_id, or all=true — refusing to guess between one show and the whole node"))
		return
	}

	// The subtitle profile every subtitle want is measured against (ADR-0085):
	// it accepts any subtitle bytes and is terminal there, so the operator names
	// no profile — a caption has no quality bar to choose. Resolved once, by name,
	// for all the wants this request creates.
	var profileID string
	if err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		var e error
		profileID, e = a.profileIDByName(r.Context(), tx, "subtitle")
		return e
	}); err != nil {
		a.fail(w, r, "quality_profile", err)
		return
	}

	videos, err := a.videosNeedingSubtitleWant(r.Context(), body.LibraryID, body.WorkID, lang)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}

	created := 0
	for _, v := range videos {
		item := desired.Item{
			ID:               a.newID(),
			WorkID:           v.workID,
			Aspect:           desired.AspectSubtitle,
			Language:         lang,
			QualityProfileID: profileID,
			// A subtitle is terminal (the provider chose the file); there is no
			// "better" caption to keep looking for, so the want does not monitor.
			Monitor: false,
			Reason:  "subtitles: " + lang + " requested",
		}
		// Scope to the precise target the satisfaction check reads (ADR-0086): an
		// episode's own video by item, a film's by edition.
		if v.itemID != "" {
			item.Scope = desired.ScopeItem
			item.ItemID = v.itemID
		} else {
			item.Scope = desired.ScopeEdition
			item.EditionID = v.editionID
		}

		if _, err := a.catalog.CreateDesiredItem(r.Context(), item); err != nil {
			if catalog.IsDuplicateWant(err) {
				continue // already wanted — the idempotent re-run.
			}
			a.fail(w, r, "desired", err)
			return
		}
		created++
	}

	a.write(w, r, http.StatusAccepted, map[string]any{
		"candidates": len(videos),
		"created":    created,
	})
}

// subtitleWantVideo is a video that should get a subtitle want: the asset's
// target, enough to build the want (ADR-0086 — item when the video is linked to
// one, edition otherwise).
type subtitleWantVideo struct {
	workID    string
	editionID string
	itemID    string
}

// videosNeedingSubtitleWant finds managed video assets in scope that hold no
// subtitle of the given language on their target, the same predicate the want's
// satisfaction reads (reconcile's subtitleAssetsForWant): a subtitle matches by
// item for an item-linked video and by edition otherwise, and its language is
// the canonical attributes.language or the sidecar filename convention. Mirroring
// that predicate is what keeps the door from creating a want the reconcile would
// immediately call satisfied.
func (a *API) videosNeedingSubtitleWant(ctx context.Context, libraryID, workID, lang string) ([]subtitleWantVideo, error) {
	where := []string{
		"a.source_class = 'managed'",
		"a.blob_hash IS NOT NULL",
		"a.mime LIKE 'video/%'",
		"a.missing_since IS NULL",
		// Not already captioned in this language, on the SAME target the want's
		// satisfaction reads: by item for an item-linked video, by edition
		// otherwise.
		`NOT EXISTS (
			SELECT 1 FROM assets s
			WHERE s.role = 'subtitle'
			  AND s.missing_since IS NULL
			  AND (
			        lower(coalesce(json_extract(s.attributes, '$.language'), '')) = ?
			     OR lower(coalesce(s.filename, '')) LIKE ?
			  )
			  AND (
			        (a.item_id IS NOT NULL AND s.item_id = a.item_id)
			     OR (a.item_id IS NULL AND s.edition_id = a.edition_id)
			  )
		)`,
	}
	args := []any{lang, "%." + lang + ".%"}
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
		SELECT e.work_id, a.edition_id, coalesce(a.item_id, '')
		FROM assets a
		JOIN editions e ON e.id = a.edition_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.filename`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []subtitleWantVideo
	for rows.Next() {
		var v subtitleWantVideo
		if err := rows.Scan(&v.workID, &v.editionID, &v.itemID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
