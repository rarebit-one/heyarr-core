package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// artworkRole is the assets.role a cover asset is given — the SAME role a shipped
// folder.jpg gets, on purpose (ADR-0087). It matches the literal `role='artwork'`
// the cover resolver selects on (internal/api/resources/browse.go artworkPick),
// so a cover heyarr fetched is indistinguishable from one a release shipped and
// needs no consumer change.
const artworkRole = "artwork"

// artworkFilename names a fetched cover so it wins artworkRank's `GLOB 'cover*'`
// tier (browse.go) — the representative cover of the Work — regardless of the
// bytes' real format.
const artworkFilename = "cover.jpg"

// defaultArtworkMIME is used when a fetch could not determine the image type. The
// cover resolver serves whatever the asset's mime says; a wrong-but-plausible
// image/* is better than an empty mime that a consumer cannot render.
const defaultArtworkMIME = "image/jpeg"

// FetchedArtwork is a cover image downloaded from an enrich source (ADR-0087): a
// role='artwork' asset whose bytes came from the network (Cover Art Archive, Open
// Library) rather than out of a release folder. It carries only what a fetch
// knows — the adopted blob, its size, its image mime and which source produced
// it.
type FetchedArtwork struct {
	BlobHash string
	Size     int64
	// MIME is the image type the fetch observed (image/jpeg, image/png, …). Empty
	// falls back to defaultArtworkMIME.
	MIME string
	// Source is the enrich source that produced the cover ("coverartarchive",
	// "openlibrary"), recorded in the asset's attributes.
	Source string
}

// RecordFetchedArtwork attaches a fetched cover to a Work's representative Edition
// as a managed role='artwork' asset (ADR-0087) — the generalisation of ADR-0084/
// 0085's recordSubtitleAsset for a different role and MIME. It is the import step
// of an enrich job: the provider returned a cover URL, the download client adopted
// its bytes into the CAS, and this lands them as an ordinary managed asset that
// replication, integrity and the cover resolver (artworkPick) treat exactly like
// a shipped folder.jpg — so every consumer serves it with no change.
//
// # It attaches at an Edition and resolves at the Work (ADR-0087 §5)
//
// artworkPick resolves a Work's cover by walking ALL its editions, so a cover on
// any one Edition serves the whole Work. This attaches it to the Work's
// representative Edition — the one carrying the Work's primary asset — so a
// multi-format album (a FLAC and an MP3 Edition of one album) gets one cover, not
// one per format.
//
// # Idempotent
//
// Keyed on (edition, blob, role): the enrich job will be re-run (invariant 9), and
// a second run produces bytes the CAS deduplicates, converging on the same asset
// row rather than adding a second one.
func (c *Catalog) RecordFetchedArtwork(
	ctx context.Context, workID string, art FetchedArtwork, now time.Time,
) error {
	if art.BlobHash == "" {
		return errors.New("catalog: a fetched artwork needs a blob")
	}
	mime := strings.TrimSpace(art.MIME)
	if mime == "" {
		mime = defaultArtworkMIME
	}

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		editionID, libraryID, err := representativeEdition(ctx, tx, workID)
		if err != nil {
			return err
		}

		stamp := now.Format(timestampFormat)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blobs (hash, size, mime, first_seen_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (hash) DO NOTHING`,
			art.BlobHash, art.Size, mime, stamp); err != nil {
			return err
		}

		var existing string
		lookErr := tx.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE edition_id = ? AND blob_hash = ? AND role = ?`,
			editionID, art.BlobHash, artworkRole).Scan(&existing)
		switch {
		case lookErr == nil:
			_, err = tx.ExecContext(ctx, `UPDATE assets SET updated_at = ? WHERE id = ?`, stamp, existing)
			return err
		case !errors.Is(lookErr, sql.ErrNoRows):
			return lookErr
		}

		attrs, err := encodeAttributes(map[string]any{"source": art.Source})
		if err != nil {
			return err
		}
		assetID := uuid.Must(uuid.NewV7()).String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, attributes,
				item_id, created_at, updated_at)
			VALUES (?, ?, ?, 'managed', ?, NULL, ?, ?, ?, 'fetched', ?, NULL, ?, ?)`,
			assetID, editionID, libraryID, art.BlobHash,
			artworkRole, artworkFilename, mime, attrs, stamp, stamp); err != nil {
			return err
		}

		var peerID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM peers WHERE is_self = 1`).Scan(&peerID); err != nil {
			return fmt.Errorf("catalog: resolving this peer for an artwork replica: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present, updated_at = excluded.updated_at`,
			art.BlobHash, peerID, art.Size, stamp, stamp); err != nil {
			return err
		}

		e, err := c.events.EmitTx(ctx, tx, events.TypeAssetCreated, "asset", assetID,
			map[string]any{
				"asset_id": assetID, "edition_id": editionID,
				"blob_hash": art.BlobHash, "role": artworkRole,
				"work_id": workID, "source": art.Source,
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

// representativeEdition resolves the Edition a Work's cover should attach to: the
// one carrying the Work's primary asset (artworkPick resolves across all a Work's
// editions, so a cover here serves the whole Work), falling back to any edition of
// the Work when no primary asset exists yet. It returns the edition id and its
// library id (nullable — an edition may not be scoped to a library).
func representativeEdition(ctx context.Context, tx *sql.Tx, workID string) (editionID string, libraryID any, err error) {
	var lib sql.NullString
	scanErr := tx.QueryRowContext(ctx, `
		SELECT a.edition_id, a.library_id
		FROM assets a
		JOIN editions e ON e.id = a.edition_id
		WHERE e.work_id = ? AND a.role = 'primary' AND a.blob_hash IS NOT NULL
		ORDER BY a.created_at, a.id
		LIMIT 1`, workID).Scan(&editionID, &lib)
	switch {
	case scanErr == nil:
		return editionID, nullableString(lib), nil
	case !errors.Is(scanErr, sql.ErrNoRows):
		return "", nil, scanErr
	}
	// No primary asset yet — attach to any edition of the Work. A book that is a
	// single epub Edition still has an edition to hang a cover on.
	if err := tx.QueryRowContext(ctx, `
		SELECT e.id, (SELECT a.library_id FROM assets a WHERE a.edition_id = e.id AND a.library_id IS NOT NULL LIMIT 1)
		FROM editions e WHERE e.work_id = ?
		ORDER BY e.created_at, e.id
		LIMIT 1`, workID).Scan(&editionID, &lib); err != nil {
		return "", nil, fmt.Errorf("catalog: the work %s has no edition to attach a cover to: %w", workID, err)
	}
	return editionID, nullableString(lib), nil
}

// WriteWorkExternalID records a canonical id for a Work in the external_ids table
// (ADR-0050) — the FIRST production writer of that table, which until now had only
// readers and test harnesses. It is idempotent on the table's (source, value,
// entity_type) uniqueness: re-running an enrich job writes the same id again and
// converges rather than erroring.
//
// entity_type is always 'work' here — an enrich source identifies the Work, not a
// particular Edition of it. An empty source or value is a no-op rather than an
// error, so a provider that returned a cover but no id does not fail the write.
func (c *Catalog) WriteWorkExternalID(ctx context.Context, workID, source, value string) error {
	source = strings.TrimSpace(source)
	value = strings.TrimSpace(value)
	if source == "" || value == "" {
		return nil
	}
	id := uuid.Must(uuid.NewV7()).String()
	_, err := c.db.Writer().ExecContext(ctx, `
		INSERT INTO external_ids (id, entity_type, entity_id, source, value)
		VALUES (?, 'work', ?, ?, ?)
		ON CONFLICT (source, value, entity_type) DO NOTHING`,
		id, workID, source, value)
	if err != nil {
		return fmt.Errorf("catalog: recording %s id for work %s: %w", source, workID, err)
	}
	return nil
}

// CorrectWorkIdentity replaces a Work's DISPLAY identity — its title, sort_title
// and author/artist attribute — with an authority's canonical values (ADR-0088),
// WITHOUT touching its work_key. The key stays the ingest heuristic's (so no
// re-keying, no merges, no dangling references); only the face a person reads and
// searches by changes. The original ingest strings are preserved under
// attributes.identified_title / identified_author (or identified_artist) so the
// correction is auditable and reversible.
//
// It is additive and guarded: an empty canonicalTitle leaves the title untouched,
// an empty canonicalAuthor leaves the author attribute untouched — a provider that
// offered no correction for a field never blanks it. The identified_* record is
// written only the FIRST time a field is corrected, so repeated enrich runs do not
// overwrite the true original with an earlier correction.
func (c *Catalog) CorrectWorkIdentity(
	ctx context.Context, workID, canonicalTitle, canonicalAuthor string, now time.Time,
) error {
	canonicalTitle = strings.TrimSpace(canonicalTitle)
	canonicalAuthor = strings.TrimSpace(canonicalAuthor)
	if canonicalTitle == "" && canonicalAuthor == "" {
		return nil
	}

	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		var contentType, title, attrsJSON string
		var year sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT content_type, title, year, attributes FROM works WHERE id = ?`, workID).
			Scan(&contentType, &title, &year, &attrsJSON); err != nil {
			return fmt.Errorf("catalog: the work to correct is gone: %w", err)
		}

		attrs := map[string]any{}
		if strings.TrimSpace(attrsJSON) != "" {
			if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
				return fmt.Errorf("catalog: decoding work %s attributes: %w", workID, err)
			}
		}

		// The author attribute is named by content type: music credits an artist,
		// a book an author (ADR-0080). A book is the live corrector; music shares
		// the path for when it arrives.
		authorKey := "author"
		if contentType == "music" {
			authorKey = "artist"
		}

		newTitle := title
		newSort := ""
		if canonicalTitle != "" && canonicalTitle != title {
			if _, ok := attrs["identified_title"]; !ok {
				attrs["identified_title"] = title
			}
			newTitle = canonicalTitle
		}
		if canonicalAuthor != "" {
			if prev, _ := attrs[authorKey].(string); prev != canonicalAuthor {
				if _, ok := attrs["identified_"+authorKey]; !ok && prev != "" {
					attrs["identified_"+authorKey] = prev
				}
				attrs[authorKey] = canonicalAuthor
			}
		}

		// Recompute sort_title from the (possibly new) title through the ONE
		// normalisation the scanner uses (identification.Describe), so a corrected
		// Work sorts beside a scanned one. work_key is deliberately discarded —
		// ADR-0088 freezes it.
		_, newSort = identification.Describe(contentType, newTitle, int(year.Int64))

		encoded, err := encodeAttributes(attrs)
		if err != nil {
			return err
		}
		stamp := now.Format(timestampFormat)
		_, err = tx.ExecContext(ctx, `
			UPDATE works SET title = ?, sort_title = ?, attributes = ?, updated_at = ?
			WHERE id = ?`,
			newTitle, newSort, encoded, stamp, workID)
		return err
	})
}
