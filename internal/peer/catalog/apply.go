package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Apply materialises one snapshot payload. It is the ONLY writer.
//
// # Full and incremental converge
//
// A full payload replaces the contents outright. An incremental payload
// upserts the rows it carries and then prunes every row whose id is absent
// from the complete id set it also carries (see [Snapshot] for why the ids are
// there). Both leave the store holding exactly the catalogue the controller
// read — which is what makes "an incremental refresh and a full rebuild of the
// same catalogue state produce identical snapshots" a property rather than a
// coincidence, and what makes the full path a genuine drift corrector rather
// than a differently-shaped guess.
//
// # Why the whole thing is one transaction
//
// A snapshot is a fact about a moment. A half-applied one is a fact about no
// moment at all, and it is precisely the artifact a peer would be left holding
// if the link dropped mid-refresh — the situation the snapshot exists for. So
// either the version advances and the contents advance with it, or neither
// does and the peer keeps serving the older, honest answer.
func (s *Store) Apply(ctx context.Context, snap *Snapshot) error {
	if !s.writable {
		return fmt.Errorf("%w: %s", ErrReadOnly, s.path)
	}
	if snap == nil {
		return errors.New("catalog: applying a nil snapshot")
	}
	if err := snap.Meta.Validate(); err != nil {
		return err
	}
	if snap.Meta.Kind == KindIncremental && snap.IDs == nil {
		return errors.New("catalog: an incremental snapshot must carry the complete id set of " +
			"every covered table, or deletions are invisible and the snapshot silently keeps " +
			"content the catalogue no longer has")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning the snapshot transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := assertAdvances(ctx, tx, snap.Meta); err != nil {
		return err
	}

	if snap.Meta.Kind == KindFull {
		if err := clearContents(ctx, tx); err != nil {
			return err
		}
	}
	if err := upsertAll(ctx, tx, snap); err != nil {
		return err
	}
	if snap.Meta.Kind == KindIncremental {
		if err := prune(ctx, tx, snap.IDs); err != nil {
			return err
		}
	}
	if err := writeMeta(ctx, tx, snap); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing the snapshot: %w", err)
	}
	committed = true
	return nil
}

// assertAdvances refuses a version that does not move forward.
func assertAdvances(ctx context.Context, tx *sql.Tx, m Meta) error {
	var current int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM snapshot_meta WHERE id = 1`).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("catalog: reading the current snapshot version: %w", err)
	}
	if m.Version <= current {
		return fmt.Errorf("%w: this peer holds version %d and was offered version %d",
			ErrStaleSnapshot, current, m.Version)
	}
	return nil
}

// clearContents empties the covered tables, children before parents.
//
// Foreign keys are ON, so the order is the correctness rather than the
// tidiness — and deleting in Covered()'s reverse is the one line that keeps
// the constraint from having to be relaxed.
func clearContents(ctx context.Context, tx *sql.Tx) error {
	tables := Covered()
	for i := len(tables) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+tables[i]); err != nil { //nolint:gosec // table names come from Covered(), not from input
			return fmt.Errorf("catalog: clearing %s: %w", tables[i], err)
		}
	}
	return nil
}

func upsertAll(ctx context.Context, tx *sql.Tx, snap *Snapshot) error {
	for _, l := range snap.Libraries {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_libraries (id, name, content_type, enabled, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				name = excluded.name, content_type = excluded.content_type,
				enabled = excluded.enabled, created_at = excluded.created_at`,
			l.ID, l.Name, l.ContentType, boolToInt(l.Enabled), formatStamp(l.CreatedAt))
		if err != nil {
			return fmt.Errorf("catalog: applying library %s: %w", l.ID, err)
		}
	}
	for _, r := range snap.LibraryRoots {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_library_roots (id, library_id, path, ingest_mode, enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				library_id = excluded.library_id, path = excluded.path,
				ingest_mode = excluded.ingest_mode, enabled = excluded.enabled,
				created_at = excluded.created_at`,
			r.ID, r.LibraryID, r.Path, r.IngestMode, boolToInt(r.Enabled), formatStamp(r.CreatedAt))
		if err != nil {
			return fmt.Errorf("catalog: applying library root %s: %w", r.ID, err)
		}
	}
	for _, w := range snap.Works {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_works
				(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				content_type = excluded.content_type, work_key = excluded.work_key,
				title = excluded.title, sort_title = excluded.sort_title,
				year = excluded.year, attributes = excluded.attributes,
				created_at = excluded.created_at, updated_at = excluded.updated_at`,
			w.ID, w.ContentType, w.WorkKey, w.Title, w.SortTitle, w.Year, w.Attributes,
			formatStamp(w.CreatedAt), formatStamp(w.UpdatedAt))
		if err != nil {
			return fmt.Errorf("catalog: applying work %s: %w", w.ID, err)
		}
	}
	for _, e := range snap.Editions {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_editions
				(id, work_id, label, edition_type, language, attributes, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				work_id = excluded.work_id, label = excluded.label,
				edition_type = excluded.edition_type, language = excluded.language,
				attributes = excluded.attributes, created_at = excluded.created_at`,
			e.ID, e.WorkID, e.Label, e.EditionType, e.Language, e.Attributes, formatStamp(e.CreatedAt))
		if err != nil {
			return fmt.Errorf("catalog: applying edition %s: %w", e.ID, err)
		}
	}
	for _, b := range snap.Blobs {
		// A snapshot from a controller that predates M5-03 carries no state at
		// all. That is 'undecided' — which is the truth about it — and never
		// 'not_required', because inventing a recorded decision nobody took is
		// precisely how the third state gets collapsed again.
		state := b.ChunkManifest
		if state == "" {
			state = manifests.StateUndecided
		}
		if !state.Valid() {
			return fmt.Errorf("catalog: applying blob %s: %q is not a chunk-manifest state",
				b.Hash, state)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_blobs (hash, size, mime, chunk_manifest, first_seen_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (hash) DO UPDATE SET
				size = excluded.size, mime = excluded.mime,
				chunk_manifest = excluded.chunk_manifest,
				first_seen_at = excluded.first_seen_at`,
			b.Hash, b.Size, b.MIME, string(state), formatStamp(b.FirstSeenAt))
		if err != nil {
			return fmt.Errorf("catalog: applying blob %s: %w", b.Hash, err)
		}
	}
	for _, a := range snap.Assets {
		var missing any
		if a.MissingSince != nil {
			missing = formatStamp(*a.MissingSince)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshot_assets
				(id, edition_id, library_id, source_class, blob_hash, source_path, fingerprint,
				 role, filename, mime, identification_source, missing_since, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				edition_id = excluded.edition_id, library_id = excluded.library_id,
				source_class = excluded.source_class, blob_hash = excluded.blob_hash,
				source_path = excluded.source_path, fingerprint = excluded.fingerprint,
				role = excluded.role, filename = excluded.filename, mime = excluded.mime,
				identification_source = excluded.identification_source,
				missing_since = excluded.missing_since,
				created_at = excluded.created_at, updated_at = excluded.updated_at`,
			a.ID, a.EditionID, a.LibraryID, a.SourceClass, a.BlobHash, a.SourcePath, a.Fingerprint,
			a.Role, a.Filename, a.MIME, a.IdentificationSource, missing,
			formatStamp(a.CreatedAt), formatStamp(a.UpdatedAt))
		if err != nil {
			return fmt.Errorf("catalog: applying asset %s: %w", a.ID, err)
		}
	}
	return nil
}

// primaryKeyOf names each covered table's identity column. Blobs are keyed by
// their digest, because that is what a blob IS (Invariant 1).
func primaryKeyOf(table string) string {
	if table == "snapshot_blobs" {
		return "hash"
	}
	return "id"
}

// prune deletes every row the controller no longer has.
//
// The difference is computed in Go rather than as a NOT IN over a temp table,
// for a reason worth stating: a temp table would be a second place the id set
// exists, and a chunked NOT IN is silently wrong (each chunk deletes the rows
// the other chunks were keeping). A catalogue's worth of ids in memory is a
// few megabytes at the sizes this system targets, and obviously correct beats
// cleverly correct on the path that removes data.
func prune(ctx context.Context, tx *sql.Tx, keep map[string][]string) error {
	tables := Covered()
	for i := len(tables) - 1; i >= 0; i-- {
		table := tables[i]
		ids, ok := keep[table]
		if !ok {
			return fmt.Errorf("catalog: the incremental snapshot carries no id set for %s — "+
				"an absent set and an empty one mean opposite things (keep nothing / keep "+
				"everything), so it is refused rather than guessed", table)
		}
		wanted := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			wanted[id] = struct{}{}
		}

		key := primaryKeyOf(table)
		held, err := heldIDs(ctx, tx, table, key)
		if err != nil {
			return err
		}
		var doomed []string
		for _, id := range held {
			if _, keepIt := wanted[id]; !keepIt {
				doomed = append(doomed, id)
			}
		}

		for _, id := range doomed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+key+` = ?`, id); err != nil { //nolint:gosec // names come from Covered(), not from input
				return fmt.Errorf("catalog: pruning %s from %s: %w", id, table, err)
			}
		}
	}
	return nil
}

// heldIDs lists the primary keys currently in one covered table.
func heldIDs(ctx context.Context, tx *sql.Tx, table, key string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+key+` FROM `+table) //nolint:gosec // names come from Covered(), not from input
	if err != nil {
		return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
	}
	return out, nil
}

// writeMeta records what the store now holds.
//
// The digest is computed over the store's CONTENTS after the apply rather than
// over the payload, so it describes what is actually there. A digest copied
// from the payload would agree with the payload by construction and would
// therefore never catch the one failure it is for: an apply that did not do
// what the payload said.
func writeMeta(ctx context.Context, tx *sql.Tx, snap *Snapshot) error {
	stored, err := readContents(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO snapshot_meta
			(id, controller_id, version, generated_at, kind, watermark, applied_at, content_digest)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			controller_id = excluded.controller_id, version = excluded.version,
			generated_at = excluded.generated_at, kind = excluded.kind,
			watermark = excluded.watermark, applied_at = excluded.applied_at,
			content_digest = excluded.content_digest`,
		snap.Meta.ControllerID, snap.Meta.Version, formatStamp(snap.Meta.GeneratedAt),
		snap.Meta.Kind, formatStamp(snap.Meta.Watermark),
		formatStamp(time.Now().UTC()), stored.ContentDigest())
	if err != nil {
		return fmt.Errorf("catalog: recording the snapshot metadata: %w", err)
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
