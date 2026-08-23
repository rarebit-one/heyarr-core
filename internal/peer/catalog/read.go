package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// querier is the shared shape of *sql.DB and *sql.Tx, so the contents can be
// read inside the apply transaction (to compute the digest of what actually
// landed) and outside it (to answer a caller) with one implementation.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Contents reads the whole snapshot back.
//
// It returns rows rather than counts on purpose. An acceptance assertion that
// compared counts would pass on a snapshot holding the same NUMBER of
// different works — which is not a hypothetical failure but the exact shape of
// a broken prune, where one row is deleted and another wrongly re-added.
//
// Available on a read-only handle, because reading is what a snapshot is for.
func (s *Store) Contents(ctx context.Context) (*Snapshot, error) {
	snap, err := readContents(ctx, s.db)
	if err != nil {
		return nil, err
	}
	meta, err := s.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	snap.Meta = meta
	return snap, nil
}

func readContents(ctx context.Context, q querier) (*Snapshot, error) {
	snap := &Snapshot{}
	if err := readLibraries(ctx, q, snap); err != nil {
		return nil, err
	}
	if err := readLibraryRoots(ctx, q, snap); err != nil {
		return nil, err
	}
	if err := readWorks(ctx, q, snap); err != nil {
		return nil, err
	}
	if err := readEditions(ctx, q, snap); err != nil {
		return nil, err
	}
	if err := readBlobs(ctx, q, snap); err != nil {
		return nil, err
	}
	if err := readAssets(ctx, q, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func readLibraries(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, name, content_type, enabled, created_at FROM snapshot_libraries ORDER BY id`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot libraries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			l       Library
			enabled int64
			created string
		)
		if err := rows.Scan(&l.ID, &l.Name, &l.ContentType, &enabled, &created); err != nil {
			return fmt.Errorf("catalog: reading snapshot libraries: %w", err)
		}
		l.Enabled = enabled == 1
		if l.CreatedAt, err = parseStamp(created); err != nil {
			return err
		}
		snap.Libraries = append(snap.Libraries, l)
	}
	return wrapRows(rows, "libraries")
}

func readLibraryRoots(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, library_id, path, ingest_mode, enabled, created_at
		 FROM snapshot_library_roots ORDER BY id`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot library roots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			r       LibraryRoot
			enabled int64
			created string
		)
		if err := rows.Scan(&r.ID, &r.LibraryID, &r.Path, &r.IngestMode, &enabled, &created); err != nil {
			return fmt.Errorf("catalog: reading snapshot library roots: %w", err)
		}
		r.Enabled = enabled == 1
		if r.CreatedAt, err = parseStamp(created); err != nil {
			return err
		}
		snap.LibraryRoots = append(snap.LibraryRoots, r)
	}
	return wrapRows(rows, "library roots")
}

func readWorks(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at
		 FROM snapshot_works ORDER BY id`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot works: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			w                Work
			year             sql.NullInt64
			created, updated string
		)
		if err := rows.Scan(&w.ID, &w.ContentType, &w.WorkKey, &w.Title, &w.SortTitle,
			&year, &w.Attributes, &created, &updated); err != nil {
			return fmt.Errorf("catalog: reading snapshot works: %w", err)
		}
		if year.Valid {
			v := year.Int64
			w.Year = &v
		}
		if w.CreatedAt, err = parseStamp(created); err != nil {
			return err
		}
		if w.UpdatedAt, err = parseStamp(updated); err != nil {
			return err
		}
		snap.Works = append(snap.Works, w)
	}
	return wrapRows(rows, "works")
}

func readEditions(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, work_id, label, edition_type, language, attributes, created_at
		 FROM snapshot_editions ORDER BY id`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot editions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			e        Edition
			language sql.NullString
			created  string
		)
		if err := rows.Scan(&e.ID, &e.WorkID, &e.Label, &e.EditionType,
			&language, &e.Attributes, &created); err != nil {
			return fmt.Errorf("catalog: reading snapshot editions: %w", err)
		}
		e.Language = nullString(language)
		if e.CreatedAt, err = parseStamp(created); err != nil {
			return err
		}
		snap.Editions = append(snap.Editions, e)
	}
	return wrapRows(rows, "editions")
}

func readBlobs(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT hash, size, mime, chunk_manifest, first_seen_at FROM snapshot_blobs ORDER BY hash`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			b     Blob
			mime  sql.NullString
			state string
			seen  string
		)
		if err := rows.Scan(&b.Hash, &b.Size, &mime, &state, &seen); err != nil {
			return fmt.Errorf("catalog: reading snapshot blobs: %w", err)
		}
		b.MIME = nullString(mime)
		if b.ChunkManifest, err = manifests.ParseState(state); err != nil {
			return fmt.Errorf("catalog: reading snapshot blobs: %w", err)
		}
		if b.FirstSeenAt, err = parseStamp(seen); err != nil {
			return err
		}
		snap.Blobs = append(snap.Blobs, b)
	}
	return wrapRows(rows, "blobs")
}

func readAssets(ctx context.Context, q querier, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, edition_id, library_id, source_class, blob_hash, source_path, fingerprint,
		        role, filename, mime, identification_source, missing_since, created_at, updated_at
		 FROM snapshot_assets ORDER BY id`)
	if err != nil {
		return fmt.Errorf("catalog: reading snapshot assets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			a                                                  Asset
			library, blob, source, fingerprint, filename, mime sql.NullString
			missing                                            sql.NullString
			created, updated                                   string
		)
		if err := rows.Scan(&a.ID, &a.EditionID, &library, &a.SourceClass, &blob, &source,
			&fingerprint, &a.Role, &filename, &mime, &a.IdentificationSource,
			&missing, &created, &updated); err != nil {
			return fmt.Errorf("catalog: reading snapshot assets: %w", err)
		}
		a.LibraryID = nullString(library)
		a.BlobHash = nullString(blob)
		a.SourcePath = nullString(source)
		a.Fingerprint = nullString(fingerprint)
		a.Filename = nullString(filename)
		a.MIME = nullString(mime)
		if missing.Valid {
			t, err := parseStamp(missing.String)
			if err != nil {
				return err
			}
			a.MissingSince = &t
		}
		if a.CreatedAt, err = parseStamp(created); err != nil {
			return err
		}
		if a.UpdatedAt, err = parseStamp(updated); err != nil {
			return err
		}
		snap.Assets = append(snap.Assets, a)
	}
	return wrapRows(rows, "assets")
}

func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func wrapRows(rows *sql.Rows, what string) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: reading snapshot %s: %w", what, err)
	}
	return nil
}

// State is what an operator sees when they ask how stale a peer is.
//
// It is a value rather than three loose return arguments because the three
// travel together everywhere they go — the API, the CLI and M7's read path all
// need the identity, the version AND the age. A caller holding two of the
// three is a caller about to present a stale answer as a current one.
type State struct {
	Meta Meta
	Age  time.Duration
}

// Describe reports the snapshot's identity, version and age at now.
//
// [ErrNoSnapshot] when there is none — never a zero State.
func (s *Store) Describe(ctx context.Context, now time.Time) (State, error) {
	m, err := s.Metadata(ctx)
	if err != nil {
		return State{}, err
	}
	return State{Meta: m, Age: m.Age(now)}, nil
}
