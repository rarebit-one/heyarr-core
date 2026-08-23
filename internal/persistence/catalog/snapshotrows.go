package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// The covered tables, and how each one says it changed.
//
// Works and assets carry updated_at; libraries, roots and editions carry only
// created_at, and blobs carry first_seen_at, because those rows do not change
// after they are written. Using the column each table actually has is more
// honest than adding updated_at everywhere for symmetry — a column that is
// always equal to created_at is a column that will eventually be read as
// meaning something.
var snapshotChangeColumn = map[string]string{
	"snapshot_libraries":     "created_at",
	"snapshot_library_roots": "created_at",
	"snapshot_works":         "updated_at",
	"snapshot_editions":      "created_at",
	"snapshot_blobs":         "first_seen_at",
	"snapshot_assets":        "updated_at",
}

// sourceTable maps a snapshot table to the controller table it mirrors and the
// key it is identified by.
var sourceTable = map[string]struct{ table, key string }{
	"snapshot_libraries":     {"libraries", "id"},
	"snapshot_library_roots": {"library_roots", "id"},
	"snapshot_works":         {"works", "id"},
	"snapshot_editions":      {"editions", "id"},
	"snapshot_blobs":         {"blobs", "hash"},
	"snapshot_assets":        {"assets", "id"},
}

// sinceClause selects rows at or after the watermark.
//
// At-or-after rather than strictly-after, deliberately. The two directions have
// asymmetric failure modes: including a row that was already sent costs one
// redundant upsert, and excluding a row that changed in the same instant as
// the last read costs a snapshot that is silently missing it, forever, until
// something else touches that row. The incremental path is an optimisation;
// the only safe direction for an optimisation to be wrong in is "too much".
func sinceClause(column string, since time.Time, incremental bool) (string, []any) {
	if !incremental {
		return "", nil
	}
	return " WHERE " + column + " >= ?", []any{stampOf(since)}
}

func readSnapshotRows(
	ctx context.Context,
	tx *sql.Tx,
	snap *peercatalog.Snapshot,
	since time.Time,
	incremental bool,
) error {
	readers := []func(context.Context, *sql.Tx, *peercatalog.Snapshot, time.Time, bool) error{
		sourceLibraries, sourceLibraryRoots, sourceWorks, sourceEditions, sourceBlobs, sourceAssets,
	}
	for _, read := range readers {
		if err := read(ctx, tx, snap, since, incremental); err != nil {
			return err
		}
	}
	return nil
}

// readSnapshotIDs collects the complete id set of every covered table.
//
// It is what makes an incremental refresh able to express DELETION at all —
// see the [peercatalog.Snapshot] doc comment for why this is cheaper and more
// correct than tombstones. It is read inside the same transaction as the rows,
// so the ids and the rows describe the same moment.
func readSnapshotIDs(ctx context.Context, tx *sql.Tx) (map[string][]string, error) {
	out := make(map[string][]string, len(peercatalog.Covered()))
	for _, table := range peercatalog.Covered() {
		src, ok := sourceTable[table]
		if !ok {
			return nil, fmt.Errorf("catalog: no controller table mirrors %s", table)
		}
		ids, err := allIDs(ctx, tx, src.table, src.key)
		if err != nil {
			return nil, err
		}
		out[table] = ids
	}
	return out, nil
}

// allIDs lists every key in one controller table.
//
// A table with no rows maps to an EMPTY slice rather than nil-that-marshals-to-
// null: the peer refuses an absent id set, because absent and empty mean
// opposite things there (keep everything / keep nothing).
func allIDs(ctx context.Context, tx *sql.Tx, table, key string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+key+` FROM `+table) //nolint:gosec // names come from a fixed map, not from input
	if err != nil {
		return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing %s: %w", table, err)
	}
	return ids, nil
}

func sourceLibraries(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_libraries"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, content_type, enabled, created_at FROM libraries`+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading libraries for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			l       peercatalog.Library
			enabled int64
			created string
		)
		if err := rows.Scan(&l.ID, &l.Name, &l.ContentType, &enabled, &created); err != nil {
			return fmt.Errorf("catalog: reading libraries for the snapshot: %w", err)
		}
		l.Enabled = enabled == 1
		if l.CreatedAt, err = parseSnapshotStamp(created); err != nil {
			return err
		}
		snap.Libraries = append(snap.Libraries, l)
	}
	return snapshotRowsErr(rows, "libraries")
}

func sourceLibraryRoots(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_library_roots"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	rows, err := tx.QueryContext(ctx,
		`SELECT id, library_id, path, ingest_mode, enabled, created_at FROM library_roots`+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading library roots for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			r       peercatalog.LibraryRoot
			enabled int64
			created string
		)
		if err := rows.Scan(&r.ID, &r.LibraryID, &r.Path, &r.IngestMode, &enabled, &created); err != nil {
			return fmt.Errorf("catalog: reading library roots for the snapshot: %w", err)
		}
		r.Enabled = enabled == 1
		if r.CreatedAt, err = parseSnapshotStamp(created); err != nil {
			return err
		}
		snap.LibraryRoots = append(snap.LibraryRoots, r)
	}
	return snapshotRowsErr(rows, "library roots")
}

func sourceWorks(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_works"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	rows, err := tx.QueryContext(ctx,
		`SELECT id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at
		 FROM works`+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading works for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			w                peercatalog.Work
			year             sql.NullInt64
			created, updated string
		)
		if err := rows.Scan(&w.ID, &w.ContentType, &w.WorkKey, &w.Title, &w.SortTitle,
			&year, &w.Attributes, &created, &updated); err != nil {
			return fmt.Errorf("catalog: reading works for the snapshot: %w", err)
		}
		if year.Valid {
			v := year.Int64
			w.Year = &v
		}
		if w.CreatedAt, err = parseSnapshotStamp(created); err != nil {
			return err
		}
		if w.UpdatedAt, err = parseSnapshotStamp(updated); err != nil {
			return err
		}
		snap.Works = append(snap.Works, w)
	}
	return snapshotRowsErr(rows, "works")
}

func sourceEditions(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_editions"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	rows, err := tx.QueryContext(ctx,
		`SELECT id, work_id, label, edition_type, language, attributes, created_at
		 FROM editions`+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading editions for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			e        peercatalog.Edition
			language sql.NullString
			created  string
		)
		if err := rows.Scan(&e.ID, &e.WorkID, &e.Label, &e.EditionType,
			&language, &e.Attributes, &created); err != nil {
			return fmt.Errorf("catalog: reading editions for the snapshot: %w", err)
		}
		if language.Valid {
			v := language.String
			e.Language = &v
		}
		if e.CreatedAt, err = parseSnapshotStamp(created); err != nil {
			return err
		}
		snap.Editions = append(snap.Editions, e)
	}
	return snapshotRowsErr(rows, "editions")
}

func sourceBlobs(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_blobs"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	// The chunk-manifest state travels with the row, computed the same way
	// ChunkManifestState computes it (M5-03, §16). A snapshot that carried the
	// old `chunked` boolean would describe a fabric one state poorer than the
	// one it came from: a peer reading it could not tell "these bytes will
	// never need a manifest" from "nobody has looked", which is the distinction
	// the whole of M5-03 exists to preserve.
	rows, err := tx.QueryContext(ctx,
		`SELECT b.hash, b.size, b.mime, b.first_seen_at,
		        CASE
		            WHEN m.blob_hash IS NOT NULL              THEN 'present'
		            WHEN b.chunking_exempt_reason IS NOT NULL THEN 'not_required'
		            ELSE 'undecided'
		        END
		 FROM blobs b
		 LEFT JOIN chunk_manifests m ON m.blob_hash = b.hash`+where+` ORDER BY b.hash`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading blobs for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			b     peercatalog.Blob
			mime  sql.NullString
			seen  string
			state string
		)
		if err := rows.Scan(&b.Hash, &b.Size, &mime, &seen, &state); err != nil {
			return fmt.Errorf("catalog: reading blobs for the snapshot: %w", err)
		}
		if mime.Valid {
			v := mime.String
			b.MIME = &v
		}
		if b.ChunkManifest, err = manifests.ParseState(state); err != nil {
			return fmt.Errorf("catalog: reading blobs for the snapshot: %w", err)
		}
		if b.FirstSeenAt, err = parseSnapshotStamp(seen); err != nil {
			return err
		}
		snap.Blobs = append(snap.Blobs, b)
	}
	return snapshotRowsErr(rows, "blobs")
}

func sourceAssets(ctx context.Context, tx *sql.Tx, snap *peercatalog.Snapshot, since time.Time, inc bool) error {
	where, args := sinceClause(snapshotChangeColumn["snapshot_assets"], since, inc)
	//nolint:gosec // the only concatenated fragment is sinceClause's, built from a fixed column map; the watermark is bound
	rows, err := tx.QueryContext(ctx,
		`SELECT id, edition_id, library_id, source_class, blob_hash, source_path, fingerprint,
		        role, filename, mime, identification_source, missing_since, created_at, updated_at
		 FROM assets`+where+` ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading assets for the snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			a                                                  peercatalog.Asset
			library, blob, source, fingerprint, filename, mime sql.NullString
			missing                                            sql.NullString
			created, updated                                   string
		)
		if err := rows.Scan(&a.ID, &a.EditionID, &library, &a.SourceClass, &blob, &source,
			&fingerprint, &a.Role, &filename, &mime, &a.IdentificationSource,
			&missing, &created, &updated); err != nil {
			return fmt.Errorf("catalog: reading assets for the snapshot: %w", err)
		}
		a.LibraryID = snapshotNullString(library)
		a.BlobHash = snapshotNullString(blob)
		a.SourcePath = snapshotNullString(source)
		a.Fingerprint = snapshotNullString(fingerprint)
		a.Filename = snapshotNullString(filename)
		a.MIME = snapshotNullString(mime)
		if missing.Valid {
			t, err := parseSnapshotStamp(missing.String)
			if err != nil {
				return err
			}
			a.MissingSince = &t
		}
		if a.CreatedAt, err = parseSnapshotStamp(created); err != nil {
			return err
		}
		if a.UpdatedAt, err = parseSnapshotStamp(updated); err != nil {
			return err
		}
		snap.Assets = append(snap.Assets, a)
	}
	return snapshotRowsErr(rows, "assets")
}

func snapshotNullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func snapshotRowsErr(rows *sql.Rows, what string) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: reading %s for the snapshot: %w", what, err)
	}
	return nil
}
