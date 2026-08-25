package backup

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// ErrIntegrity is a snapshot that opens but is not internally consistent —
// integrity_check or foreign_key_check found something. A backup that restores
// to a corrupt database is worse than one that fails to restore.
var ErrIntegrity = errors.New("backup: the snapshot failed an integrity check")

// ErrNotReadOnly is a read handle that is not actually read-only — the DSN did
// not take. It is the same class of failure catalog.assertReadOnly guards, and
// here too the pragma IS the invariant-5 property rather than a correctness aid.
var ErrNotReadOnly = errors.New("backup: the snapshot handle is not query_only — invariant 5 is not held")

// OpenOptions configure verification.
type OpenOptions struct {
	// PublicKey, when set, requires the manifest to carry a signature that
	// verifies against it. When nil, the signature is not checked — a
	// single-node open of one's own backup — but the digest always is.
	PublicKey ed25519.PublicKey
	// ExpectSchema, when positive, requires the backup's schema version to equal
	// it. A restore into a binary at a different version is refused
	// ([ErrSchemaMismatch]): a silent schema mismatch corrupts.
	ExpectSchema int64
}

// Opened is a verified backup, open for reading only.
type Opened struct {
	db       *sql.DB
	manifest Manifest
}

// Manifest is the verified manifest.
func (o *Opened) Manifest() Manifest { return o.manifest }

// DB is a read-only handle to the snapshot. A write through it fails at the
// storage layer with SQLITE_READONLY (invariant 5).
func (o *Opened) DB() *sql.DB { return o.db }

// Close releases the handle.
func (o *Opened) Close() error { return o.db.Close() }

// Open verifies a backup directory and opens its snapshot read-only.
//
// The order is deliberate: cheap structural checks, then the digest (which
// re-derives trust from invariant 1's hash), then the signature if required,
// then the schema expectation, and only then does it open the file and prove it
// is internally consistent. A backup that fails any of these is refused before
// anything downstream can act on it.
func Open(ctx context.Context, dir string, opts OpenOptions) (*Opened, error) {
	manifest, err := verify(dir, opts)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", readerDSN(filepath.Join(dir, SnapshotFile)))
	if err != nil {
		return nil, fmt.Errorf("backup: opening the snapshot read-only: %w", err)
	}
	opened := &Opened{db: db, manifest: manifest}
	if err := opened.assertReadOnly(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := assertConsistent(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := assertSchema(ctx, db, manifest.SchemaVersion); err != nil {
		_ = db.Close()
		return nil, err
	}
	return opened, nil
}

// verify runs every check that does not need the database open.
func verify(dir string, opts OpenOptions) (Manifest, error) {
	manifest, err := ReadManifest(dir)
	if err != nil {
		return Manifest{}, err
	}

	want, err := hashing.Parse(manifest.Digest)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: the manifest digest is malformed: %w", err)
	}
	got, size, err := hashing.HashFile(filepath.Join(dir, SnapshotFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: hashing the snapshot: %w", err)
	}
	if size != manifest.SizeBytes {
		return Manifest{}, fmt.Errorf("backup: snapshot is %d bytes, manifest says %d", size, manifest.SizeBytes)
	}
	if !got.Equal(want) {
		return Manifest{}, fmt.Errorf("%w: snapshot digest %s does not match manifest %s",
			ErrSignatureInvalid, got, want)
	}

	if opts.PublicKey != nil {
		if err := manifest.Verify(opts.PublicKey); err != nil {
			return Manifest{}, err
		}
	}
	if opts.ExpectSchema > 0 && manifest.SchemaVersion != opts.ExpectSchema {
		return Manifest{}, fmt.Errorf("%w: backup is at %d, this binary expects %d",
			ErrSchemaMismatch, manifest.SchemaVersion, opts.ExpectSchema)
	}
	return manifest, nil
}

// assertReadOnly proves the handle really cannot write, from the handle itself.
func (o *Opened) assertReadOnly(ctx context.Context) error {
	var queryOnly int
	if err := o.db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return fmt.Errorf("backup: verifying the snapshot is read-only: %w", err)
	}
	if queryOnly != 1 {
		return ErrNotReadOnly
	}
	return nil
}

// assertConsistent runs integrity_check and foreign_key_check.
//
// A backup taken while writes are in flight must open, its foreign keys must
// check out, and integrity_check must pass — otherwise "backup" means "a file
// that is only correct on a quiet database", which is a situation nobody needs
// one in.
func assertConsistent(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("backup: running integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("%w: integrity_check reported %q", ErrIntegrity, result)
	}

	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("backup: running foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return fmt.Errorf("%w: foreign_key_check found a violation", ErrIntegrity)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: reading foreign_key_check: %w", err)
	}
	return nil
}

// assertSchema cross-checks the snapshot's real applied version against the
// manifest's claim.
//
// The manifest already carries the version, and a valid signature makes that
// claim authentic — but an unsigned backup's manifest is just a file, and a
// backup whose bytes are at a different schema than its manifest says is one
// [Open] should refuse rather than open at a version it will then act on wrongly.
func assertSchema(ctx context.Context, db *sql.DB, claimed int64) error {
	actual, err := schemaVersionOf(ctx, db)
	if err != nil {
		return err
	}
	if actual != claimed {
		return fmt.Errorf("%w: the snapshot is at schema %d, its manifest claims %d",
			ErrSchemaMismatch, actual, claimed)
	}
	return nil
}

// schemaVersionOf reads the applied goose version from a raw handle, mirroring
// sqlite.AppliedSchemaVersion's "most recently recorded applied row wins" rule
// so a snapshot and the live database report the same number.
func schemaVersionOf(ctx context.Context, db *sql.DB) (int64, error) {
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version'`).
		Scan(&exists); err != nil {
		return 0, fmt.Errorf("backup: looking for the migration table: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version_id, is_applied FROM goose_db_version ORDER BY id DESC`)
	if err != nil {
		return 0, fmt.Errorf("backup: reading the snapshot schema version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			version int64
			applied bool
		)
		if err := rows.Scan(&version, &applied); err != nil {
			return 0, fmt.Errorf("backup: reading the snapshot schema version: %w", err)
		}
		if applied {
			return version, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("backup: reading the snapshot schema version: %w", err)
	}
	return 0, nil
}

// readerDSN opens a snapshot without write access. mode=ro opens the file
// read-only; query_only refuses a write statement before it reaches the file.
// Both, because they fail at different layers — the same belt-and-braces
// catalog.readerDSN uses, and here the property they enforce is invariant 5.
func readerDSN(path string) string {
	return fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(true)"+
		"&_pragma=foreign_keys(ON)", path, busyTimeout.Milliseconds())
}

// RestoreOptions configure a restore.
type RestoreOptions struct {
	// PublicKey / ExpectSchema are the verification applied before anything is
	// installed — a restore verifies exactly what an [Open] does.
	PublicKey    ed25519.PublicKey
	ExpectSchema int64
	// AgainstGeneration, when positive, refuses a backup whose generation is
	// less than it ([ErrGenerationRegressed]) — restoring the stalest copy over
	// a fresher database is silent data loss (ADR-0044 question 3). Zero (a bare
	// disk with nothing to lose) restores unconditionally.
	AgainstGeneration int64
	// Clock stamps the restore.performed event. Nil uses the wall clock; a test
	// injects its own (ADR-0017).
	Clock Clock
}

func (o RestoreOptions) clock() Clock {
	if o.Clock != nil {
		return o.Clock
	}
	return wallClock{}
}

// wallClock is the default restore clock. A one-shot restore reads real time;
// the cadence and the tests inject their own.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// Restore verifies the backup at dir and installs its snapshot as a control
// database at destPath.
//
// It writes to a DIFFERENT path than any live control plane by construction —
// the caller names destPath — which is what keeps invariant 5 held: a backup is
// never opened writable in place, only copied out to become a new node's
// database. The copy is atomic (temp then rename) so an interrupted restore
// leaves either the old database or the new one, never half of each.
func Restore(ctx context.Context, dir, destPath string, opts RestoreOptions) (Manifest, error) {
	manifest, err := verify(dir, OpenOptions{PublicKey: opts.PublicKey, ExpectSchema: opts.ExpectSchema})
	if err != nil {
		return Manifest{}, err
	}
	if opts.AgainstGeneration > 0 && manifest.Generation < opts.AgainstGeneration {
		return Manifest{}, fmt.Errorf("%w: backup generation %d < current %d",
			ErrGenerationRegressed, manifest.Generation, opts.AgainstGeneration)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return Manifest{}, fmt.Errorf("backup: preparing the restore directory: %w", err)
	}
	tmp := destPath + ".restoring"
	if err := copyFile(filepath.Join(dir, SnapshotFile), tmp); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return Manifest{}, fmt.Errorf("backup: installing the restored database: %w", err)
	}

	// The restored database records its own restoration as its first act
	// (invariant 7: a restore performed is a state transition). This is written
	// into the restored plane itself, where the fact belongs — "this database
	// was restored from generation N at time T" — rather than into the operator's
	// terminal, where it would not survive. A failure here does not undo a
	// successful install: the bytes are in place, so the restore succeeded, and
	// the missing event is reported rather than pretended away.
	if err := recordRestore(ctx, destPath, manifest, opts.clock().Now()); err != nil {
		return manifest, fmt.Errorf("backup: the database was restored but its restore event could not be recorded: %w", err)
	}
	return manifest, nil
}

// recordRestore opens the freshly restored database and emits the
// restore.performed transition into its own event log.
func recordRestore(ctx context.Context, destPath string, m Manifest, restoredAt time.Time) error {
	db, err := sqlite.Open(ctx, sqlite.Options{Path: destPath})
	if err != nil {
		return fmt.Errorf("opening the restored database to record the restore: %w", err)
	}
	defer func() { _ = db.Close() }()
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	_, err = log.Emit(ctx, EventRestored, subjectRestore, generationName(m.Generation), map[string]any{
		"source_peer_id":    m.SourcePeerID,
		"source_generation": m.Generation,
		"source_taken_at":   m.TakenAt,
		"restored_at":       restoredAt,
		"source_schema":     m.SchemaVersion,
	})
	return err
}

// copyFile copies src to dst, syncing so a crash after it returns leaves the
// bytes on disk rather than in a cache the restore is about to depend on.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is a snapshot path this package built
	if err != nil {
		return fmt.Errorf("backup: opening the snapshot to restore: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // dst is a restore path the caller named
	if err != nil {
		return fmt.Errorf("backup: creating the restored database: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("backup: copying the snapshot: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("backup: syncing the restored database: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: closing the restored database: %w", err)
	}
	return nil
}
