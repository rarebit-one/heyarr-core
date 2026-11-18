package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// A work's copies: the files holding the same picture (#885, ADR-0111).
//
// Two routes ask "what other files of this are there": the cast route, which
// offers a smaller copy when a link is too thin for the one asked for
// (ADR-0111's smaller_copy), and the planner, which chooses a copy when a
// client names a work rather than an asset. They ask through one query, so
// "a copy of the same thing" means one thing.
//
// "The same thing" is the same episode when both files know their item, or
// any copy of a film. A season edition holds every episode, so "same work"
// alone would offer a different episode.

// workCopy is one primary-role file holding bytes, with what its probe found.
type workCopy struct {
	AssetID      string
	EditionID    string
	EditionLabel string
	// Probed is false when nothing has probed the blob; Picture is false when
	// it was probed and holds no video — not a copy of a picture at all.
	Probed  bool
	Picture bool
	Video   probe.Stream
	PictureFacts
}

// copy renders the row in selection's shape.
func (c workCopy) copy() playback.Copy {
	out := playback.Copy{AssetID: c.AssetID, Known: c.Probed && c.Picture}
	if out.Known {
		out.Height, out.HDR, out.VideoCodec = c.Video.Height, c.Video.HDR(), c.Video.Codec
	}
	if c.BitrateBPS != nil {
		out.BitrateBPS = *c.BitrateBPS
	}
	return out
}

// queryCopies lists the present, byte-holding, primary-role assets matching
// scope (a predicate over a2, the candidate asset, e2 its edition and w its
// work), ordered by id. The guest boundary (ADR-0074) applies as it does to
// every other asset read: a Guest is never offered a vault file.
func (a *API) queryCopies(ctx context.Context, scope string, args ...any) ([]workCopy, error) {
	where := []string{
		scope,
		"a2.role = 'primary'",
		"a2.blob_hash IS NOT NULL",
		"a2.missing_since IS NULL",
	}
	if frag, fargs := guestAssetFilterCtx(ctx, "a2.source_class"); frag != "" {
		where = append(where, frag)
		args = append(args, fargs...)
	}
	//nolint:gosec // assembled only from literal fragments; every value is bound
	rows, err := a.reader.QueryContext(ctx, `
		SELECT a2.id, a2.edition_id, e2.label, bp.blob_hash IS NOT NULL, bp.bitrate_bps, bp.streams
		FROM assets a2
		JOIN editions e2 ON e2.id = a2.edition_id
		JOIN works w ON w.id = e2.work_id
		LEFT JOIN blob_probes bp ON bp.blob_hash = a2.blob_hash
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY a2.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []workCopy
	for rows.Next() {
		var (
			c       workCopy
			bitrate sql.NullInt64
			streams sql.NullString
		)
		if err := rows.Scan(&c.AssetID, &c.EditionID, &c.EditionLabel, &c.Probed, &bitrate, &streams); err != nil {
			return nil, err
		}
		c.PictureFacts = pictureFacts(bitrate, streams)
		c.Video, c.Picture = probedVideo(streams)
		out = append(out, c)
	}
	return out, rows.Err()
}

// siblingsOf is scope for the other copies of the thing assetID holds.
const siblingsOf = `a2.id != ? AND EXISTS (
		SELECT 1 FROM assets a1 JOIN editions e1 ON e1.id = a1.edition_id
		WHERE a1.id = ? AND e1.work_id = w.id
		  AND ((a1.item_id IS NOT NULL AND a2.item_id = a1.item_id)
		       OR (a1.item_id IS NULL AND w.content_type = 'movie')))`

// copiesOfFilm is scope for every copy of a film, optionally within one
// edition (an empty edition id matches all of them).
const copiesOfFilm = `w.id = ? AND w.content_type = 'movie' AND (? = '' OR a2.edition_id = ?)`

// probedVideo reads the first moving-picture stream out of a blob_probes
// streams value. A value that does not parse reads as no picture, as in
// pictureFacts.
func probedVideo(streams sql.NullString) (probe.Stream, bool) {
	if !streams.Valid {
		return probe.Stream{}, false
	}
	var parsed []probe.Stream
	if json.Unmarshal([]byte(streams.String), &parsed) != nil {
		return probe.Stream{}, false
	}
	v, ok := (probe.Result{Streams: parsed}).VideoStream()
	return v, ok && v.Height > 0
}
