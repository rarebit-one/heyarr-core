package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Embedded covers (ADR-0105): which held books still need their own file looked
// inside for a cover, and the bookkeeping of having looked.

// CoverCandidate is one book file the cover beat should enqueue an extract_cover
// job for.
type CoverCandidate struct {
	AssetID  string
	BlobHash string
	MIME     string
}

// Cover extraction outcomes, recorded per blob in cover_extractions.
const (
	CoverOutcomeExtracted = "extracted"
	CoverOutcomeNone      = "none"
)

// coverExtractDedupePrefix must equal cover.DedupeKey's prefix; the due query
// reads the job table with it so a candidate whose job is already queued — and,
// on a node without pdftoppm, waiting — does not hold a batch slot every pass.
// TestCoverDedupePrefixMatchesTheJob keeps the two in step.
const coverExtractDedupePrefix = "extract-cover:"

// DueCoverExtractions lists book files to look inside for a cover, at most limit.
//
// A file qualifies when it is the held primary of a book Work, is an EPUB or a
// PDF, has never been looked inside (no cover_extractions row for its bytes), has
// no extraction job live, and its Work has no cover of its own yet — no shipped
// cover and no extracted one. A cover FETCHED from an enrich source does not
// count: the file's own cover is preferred (ADR-0105), so a book Open Library
// already covered is still looked inside once.
//
// EPUBs sort first: they need no capability, so on a node without pdftoppm the
// waiting PDF jobs never starve the EPUBs behind them.
func (c *Catalog) DueCoverExtractions(ctx context.Context, limit int) ([]CoverCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT a.id, a.blob_hash, lower(a.mime)
		FROM works w
		JOIN editions e ON e.work_id = w.id
		JOIN assets a ON a.edition_id = e.id
		WHERE w.content_type = 'book'
		  AND a.role = 'primary' AND a.source_class = 'managed'
		  AND a.blob_hash IS NOT NULL AND a.missing_since IS NULL
		  AND lower(a.mime) IN ('application/epub+zip', 'application/pdf')
		  AND NOT EXISTS (SELECT 1 FROM cover_extractions x WHERE x.blob_hash = a.blob_hash)
		  AND NOT EXISTS (
			SELECT 1 FROM jobs j
			WHERE j.dedupe_key = ? || a.blob_hash AND j.state IN ('pending', 'leased'))
		  AND NOT EXISTS (
			SELECT 1 FROM editions e2 JOIN assets aw ON aw.edition_id = e2.id
			WHERE e2.work_id = w.id AND aw.role = 'artwork'
			  AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
			  AND coalesce(aw.identification_source, '') <> 'fetched')
		ORDER BY (lower(a.mime) = 'application/epub+zip') DESC, a.id
		LIMIT ?`, coverExtractDedupePrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing books due a cover extraction: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]struct{}{}
	var out []CoverCandidate
	for rows.Next() {
		var cc CoverCandidate
		if err := rows.Scan(&cc.AssetID, &cc.BlobHash, &cc.MIME); err != nil {
			return nil, fmt.Errorf("catalog: reading books due a cover extraction: %w", err)
		}
		if _, dup := seen[cc.BlobHash]; dup {
			continue
		}
		seen[cc.BlobHash] = struct{}{}
		out = append(out, cc)
	}
	return out, rows.Err()
}

// RecordExtractedCover attaches a cover lifted out of a book file to that file's
// Edition, as a managed role='artwork' asset marked identification_source=
// 'extracted', and records that the bytes were looked inside. Idempotent, like
// RecordFetchedArtwork: a re-run converges on the same asset row.
func (c *Catalog) RecordExtractedCover(
	ctx context.Context, sourceAssetID, format string, art FetchedArtwork, now time.Time,
) error {
	if art.BlobHash == "" {
		return errors.New("catalog: an extracted cover needs a blob")
	}
	mime := strings.TrimSpace(art.MIME)
	if mime == "" {
		mime = defaultArtworkMIME
	}

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var editionID, workID, sourceBlob string
		var lib sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT a.edition_id, a.library_id, e.work_id, coalesce(a.blob_hash, '')
			FROM assets a JOIN editions e ON e.id = a.edition_id
			WHERE a.id = ?`, sourceAssetID).Scan(&editionID, &lib, &workID, &sourceBlob); err != nil {
			return fmt.Errorf("catalog: resolving the book asset %s for its cover: %w", sourceAssetID, err)
		}
		e, ok, err := c.recordArtworkTx(ctx, tx, editionID, nullableString(lib), workID, art, mime, "extracted", now)
		if err != nil {
			return err
		}
		if ok {
			pending = append(pending, e)
		}
		return recordCoverAttemptTx(ctx, tx, sourceBlob, CoverOutcomeExtracted, format, "", now)
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// RecordCoverAttempt records that a book file was looked inside and yielded no
// cover, so the beat does not open it again. detail is a short reason for an
// operator; it is truncated rather than rejected.
func (c *Catalog) RecordCoverAttempt(ctx context.Context, blobHash, format, detail string, now time.Time) error {
	if blobHash == "" {
		return errors.New("catalog: a cover attempt needs a blob")
	}
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		return recordCoverAttemptTx(ctx, tx, blobHash, CoverOutcomeNone, format, detail, now)
	})
}

// maxCoverDetail bounds the stored reason; a tool's error output can be long.
const maxCoverDetail = 500

func recordCoverAttemptTx(ctx context.Context, tx *sql.Tx, blobHash, outcome, format, detail string, now time.Time) error {
	if len(detail) > maxCoverDetail {
		detail = detail[:maxCoverDetail]
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO cover_extractions (blob_hash, outcome, format, detail, attempted_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (blob_hash) DO UPDATE SET
			outcome = excluded.outcome, format = excluded.format,
			detail = excluded.detail, attempted_at = excluded.attempted_at`,
		blobHash, outcome, format, detail, now.Format(timestampFormat))
	return err
}
