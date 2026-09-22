package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// extractedSubtitleRole is the assets.role a subtitle asset is given — the SAME
// role an external .srt sidecar gets, on purpose. It matches the literal
// `role = 'subtitle'` the caption resolver selects on
// (internal/api/resources/renderers.go) and the value
// internal/domain/identification assigns a sidecar, so a subtitle Heyarr
// produced is indistinguishable from one the release shipped and needs no
// consumer change.
const extractedSubtitleRole = "subtitle"

// subRipMIME is what a produced subtitle is served as. It is one of the text
// types internal/api/render.servableCaptionMIME whitelists, so the DLNA
// CaptionInfo.sec header and the in-app caption list both accept it.
const subRipMIME = "application/x-subrip"

// ExtractedSubtitle is one embedded track lifted out into a SubRip blob.
type ExtractedSubtitle struct {
	// BlobHash is the extracted .srt already adopted into the CAS.
	BlobHash string
	Size     int64
	// Language is the ISO code from the track's tags, "" when it was untagged.
	Language string
	// Forced marks a foreign-dialogue-only track.
	Forced bool
	// Title is the track's own label ("SDH", "Commentary"), "" when none.
	Title string
}

// FetchedSubtitle is a subtitle downloaded from a provider (ADR-0085): a
// role='subtitle' asset whose bytes came from the network rather than out of the
// video's container. It carries only what a fetch knows — the adopted blob and
// the language — because the provider adapter has already chosen the file and
// there is nothing per-track (a forced flag, a track title) to record.
type FetchedSubtitle struct {
	BlobHash string
	Size     int64
	Language string
}

// subtitleRecord is the shape recordSubtitleAsset writes, common to an extracted
// (ADR-0084) and a fetched (ADR-0085) subtitle. The two differ only in where the
// bytes came from — the identification source, the `source` attribute — and in
// the per-track fields extraction has and a fetch does not.
type subtitleRecord struct {
	blobHash             string
	size                 int64
	language             string
	forced               bool
	title                string
	source               string // "embedded" | "opensubtitles"
	identificationSource string // "extracted" | "fetched"
}

// RecordExtractedSubtitle attaches a subtitle lifted out of a video to the SAME
// Edition as that video (the twin of RecordDerived, for a different role).
//
// # It is an ordinary subtitle asset, not a cache entry
//
// Like a remux (see RecordDerived), an extracted subtitle IS "a usable local
// representation" of the Edition (§11): recording it as a managed asset means
// replication (M4), integrity and garbage collection treat it like every other
// asset with no special case, and — the point of the whole feature — the
// caption resolver and DLNA path pick it up with no change because it is byte-
// identical in shape to a sidecar the release shipped.
//
// # Idempotent
//
// Keyed on (edition, blob, role): the extraction job will be re-run (invariant
// 9), and a second run produces bytes the CAS deduplicates, converging on the
// same asset row rather than adding a second one. Two DIFFERENT tracks are
// different bytes and so different blobs, and get their own rows — which is
// correct, they are different subtitles.
func (c *Catalog) RecordExtractedSubtitle(
	ctx context.Context, sourceAssetID string, sub ExtractedSubtitle, now time.Time,
) error {
	if sub.BlobHash == "" {
		return errors.New("catalog: an extracted subtitle needs a blob")
	}
	return c.recordSubtitleAsset(ctx, sourceAssetID, subtitleRecord{
		blobHash: sub.BlobHash, size: sub.Size, language: sub.Language,
		forced: sub.Forced, title: sub.Title,
		source: "embedded", identificationSource: "extracted",
	}, now)
}

// RecordFetchedSubtitle attaches a subtitle fetched from a provider to the SAME
// Edition as the video it captions (the twin of RecordExtractedSubtitle, for
// bytes that came from the network — ADR-0085). It is the import step of the
// subtitle fetch: the provider chose the file, the download client adopted its
// bytes into the CAS, and this lands them as an ordinary managed subtitle asset
// that replication, integrity and the caption path treat exactly like an
// extracted one or a shipped sidecar.
func (c *Catalog) RecordFetchedSubtitle(
	ctx context.Context, sourceAssetID string, sub FetchedSubtitle, now time.Time,
) error {
	if sub.BlobHash == "" {
		return errors.New("catalog: a fetched subtitle needs a blob")
	}
	return c.recordSubtitleAsset(ctx, sourceAssetID, subtitleRecord{
		blobHash: sub.BlobHash, size: sub.Size, language: sub.Language,
		source: "opensubtitles", identificationSource: "fetched",
	}, now)
}

// recordSubtitleAsset is the shared body of the two record paths: attach a
// subtitle blob as a role='subtitle' asset on the source video's Edition,
// carrying the video's item_id (ADR-0086) and a filename that keeps the caption
// resolver's stem match, with a self-peer replica. Idempotent on
// (edition, blob, role).
func (c *Catalog) recordSubtitleAsset(
	ctx context.Context, sourceAssetID string, rec subtitleRecord, now time.Time,
) error {
	var pending []events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var editionID, libraryID, sourceName, sourceItem sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT edition_id, library_id, filename, item_id FROM assets WHERE id = ?`, sourceAssetID).
			Scan(&editionID, &libraryID, &sourceName, &sourceItem); err != nil {
			return fmt.Errorf("catalog: the source video for a subtitle is gone: %w", err)
		}
		if !editionID.Valid {
			return fmt.Errorf("catalog: the source video %s has no edition to attach a subtitle to", sourceAssetID)
		}

		stamp := now.Format(timestampFormat)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blobs (hash, size, mime, first_seen_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (hash) DO NOTHING`,
			rec.blobHash, rec.size, subRipMIME, stamp); err != nil {
			return err
		}

		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE edition_id = ? AND blob_hash = ? AND role = ?`,
			editionID.String, rec.blobHash, extractedSubtitleRole).Scan(&existing)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE assets SET updated_at = ? WHERE id = ?`, stamp, existing)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		attrs, err := encodeAttributes(subtitleAttributes(rec))
		if err != nil {
			return err
		}
		assetID := uuid.Must(uuid.NewV7()).String()
		// item_id is carried over from the source video (ADR-0086): a subtitle
		// belongs to the same episode as the video it captions, so it is linked
		// to the same Item with no new knowledge — NULL when the source video has
		// none (a film, or a pre-ADR-0086 asset).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, attributes,
				item_id, created_at, updated_at)
			VALUES (?, ?, ?, 'managed', ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
			assetID, editionID.String, nullableString(libraryID), rec.blobHash,
			extractedSubtitleRole, subtitleFilename(sourceName.String, rec), subRipMIME,
			rec.identificationSource, attrs, nullableString(sourceItem), stamp, stamp); err != nil {
			return err
		}

		var peerID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM peers WHERE is_self = 1`).Scan(&peerID); err != nil {
			return fmt.Errorf("catalog: resolving this peer for a subtitle replica: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present, updated_at = excluded.updated_at`,
			rec.blobHash, peerID, rec.size, stamp, stamp); err != nil {
			return err
		}

		e, err := c.events.EmitTx(ctx, tx, events.TypeAssetCreated, "asset", assetID,
			map[string]any{
				"asset_id": assetID, "edition_id": editionID.String,
				"blob_hash": rec.blobHash, "role": extractedSubtitleRole,
				"derived_from": sourceAssetID, "language": rec.language, "source": rec.source,
			})
		if err != nil {
			return err
		}
		pending = append(pending, e)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// subtitleAttributes records where the subtitle came from and how to label it,
// in the same attributes column an ingested sidecar uses, so a consumer reading
// language/forced off an asset finds them regardless of the subtitle's origin.
func subtitleAttributes(rec subtitleRecord) map[string]any {
	attrs := map[string]any{"source": rec.source}
	if rec.language != "" {
		attrs["language"] = rec.language
	}
	if rec.forced {
		attrs["forced"] = true
	}
	if rec.title != "" {
		attrs["title"] = rec.title
	}
	return attrs
}

// subtitleFilename names the sidecar after its video, with the language (and a
// forced marker) as the sidecar convention writes it — "<video stem>.en.srt".
// The stem must remain a PREFIX of the video's stem so the caption resolver's
// stem match (renderers.go) binds it to the video, which stripping only the
// video's final extension preserves.
func subtitleFilename(sourceName string, rec subtitleRecord) string {
	base := sourceName
	if i := lastDot(base); i > 0 {
		base = base[:i]
	}
	if base == "" {
		base = "subtitles"
	}
	if rec.language != "" {
		base += "." + rec.language
	}
	if rec.forced {
		base += ".forced"
	}
	return base + ".srt"
}
