package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// goose keeps its filesystem, dialect and logger in package-level globals, so
// configuring them per call is a data race the moment two callers overlap —
// which they do: the controller migrates while anything else reads the schema
// version. Configure once.
var gooseOnce struct {
	sync.Once
	err error
}

func configureGoose() error {
	gooseOnce.Do(func() {
		goose.SetBaseFS(migrationsFS)
		goose.SetLogger(goose.NopLogger())
		gooseOnce.err = goose.SetDialect("sqlite3")
	})
	return gooseOnce.err
}

// ErrSchemaNewerThanBinary is returned when the database has been migrated by a
// newer build than the one now trying to open it.
var ErrSchemaNewerThanBinary = errors.New("sqlite: database schema is newer than this binary")

// Migrate brings the database up to the schema this binary was built with.
//
// It refuses to run when the database is at a *higher* version than the binary
// knows about. That situation means someone has downgraded — and an old binary
// writing against a newer schema does not fail loudly, it writes plausible rows
// that violate constraints the new schema depends on. By the time anyone
// notices, the damage predates every backup they still have. Refusing to start
// is the only safe response (§49).
func Migrate(ctx context.Context, db *DB) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("sqlite: selecting migration dialect: %w", err)
	}

	known, err := maxKnownVersion()
	if err != nil {
		return err
	}

	// goose creates its bookkeeping table on first use, so a database with no
	// version yet is version 0 rather than an error.
	current, err := SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("sqlite: reading schema version: %w", err)
	}
	if current > known {
		return fmt.Errorf("%w: database is at version %d, this binary knows up to %d — "+
			"it was migrated by a newer build of heyarr; upgrade rather than downgrade, "+
			"because an older binary writing against a newer schema corrupts it silently",
			ErrSchemaNewerThanBinary, current, known)
	}

	// AllowMissing is required, not optional, because this project deliberately
	// fills reserved gaps in the migration numbering rather than always
	// appending (see 00021_release_candidate_source.sql's own note: a gap "reads
	// as a DELETED migration", so the lowest free number is taken instead). That
	// policy MINTS out-of-order migrations by design — a migration numbered below
	// a version some database has already passed. Stock goose refuses those
	// ("found N missing migrations before current version"), which turns the
	// policy into an upgrade that fails on exactly the databases that skipped the
	// gap. Allowing missing migrations makes the migrator honour the numbering
	// policy: a gap-filler is applied wherever a database has not yet seen it,
	// and appended migrations behave exactly as before. It is safe here because
	// migrations are independent DDL keyed by version, not a linear replay that
	// assumes strict order.
	if err := goose.UpContext(ctx, db.Writer(), "migrations", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("sqlite: applying migrations: %w", err)
	}

	applied, err := SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("sqlite: reading schema version after migrating: %w", err)
	}
	db.log.Info("database schema ready", "version", applied, "path", db.path)
	return nil
}

// MigrateDown rolls back exactly one migration. Provided for tests and for
// recovery; it is deliberately not wired to a command, because a downgrade that
// is one keystroke away is a downgrade that happens by accident.
func MigrateDown(ctx context.Context, db *DB) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("sqlite: selecting migration dialect: %w", err)
	}
	if err := goose.DownContext(ctx, db.Writer(), "migrations"); err != nil {
		return fmt.Errorf("sqlite: rolling back migration: %w", err)
	}
	return nil
}

// SchemaVersion reports the HIGHEST migration applied to the database, which is
// the number the drift check (#150) and the newer-than-binary guard compare
// against KnownSchemaVersion.
//
// It is deliberately not goose's own GetDBVersion, which answers "the most
// RECENTLY applied migration". The two agree until a gap-filler lands: the
// numbering policy above mints out-of-order migrations, and a database at 42
// that then applies 00022 has 22 as its most recent row while its schema is
// every migration through 42. Reporting 22 there would make GET /api/v1/system
// call a fully migrated node twenty migrations "behind" (seen on the reference
// host with 00022_membership_ops). A database with no goose table yet is at 0.
func SchemaVersion(ctx context.Context, db *DB) (int64, error) {
	if err := configureGoose(); err != nil {
		return 0, err
	}
	// GetDBVersion creates the version table when it is missing, which is the
	// side effect a fresh database needs before the MAX below can be asked.
	if _, err := goose.GetDBVersionContext(ctx, db.Writer()); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := db.Writer().QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM `+goose.TableName()+` WHERE is_applied = 1`).Scan(&v); err != nil {
		return 0, fmt.Errorf("sqlite: reading the highest applied migration: %w", err)
	}
	return v.Int64, nil
}

// KnownSchemaVersion is the highest migration compiled into this binary.
//
// It is the "expected" side of the schema drift check (#150): a binary that
// embeds migrations up to 18 running against a database at 11 is seven
// migrations behind, and that is a number rather than a boolean because seven
// unapplied migrations and one are not the same operational problem.
//
// Derived from the embedded filenames, so it cannot drift from what actually
// ships — a hand-maintained constant would be wrong the first time somebody
// adds a migration and forgets it.
func KnownSchemaVersion() (int64, error) { return maxKnownVersion() }

// maxKnownVersion is the highest migration compiled into this binary, derived
// from the embedded filenames so it cannot drift from what actually ships.
func maxKnownVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("sqlite: reading embedded migrations: %w", err)
	}
	var maxVersion int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numeric, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return 0, fmt.Errorf("sqlite: migration %q does not start with a version number", e.Name())
		}
		v, err := strconv.ParseInt(numeric, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("sqlite: migration %q has an unparseable version: %w", e.Name(), err)
		}
		if v > maxVersion {
			maxVersion = v
		}
	}
	if maxVersion == 0 {
		return 0, errors.New("sqlite: no migrations are embedded in this binary")
	}
	return maxVersion, nil
}

// AppliedSchemaVersion reports the applied version without touching anything.
//
// SchemaVersion goes through goose, and goose CREATES its bookkeeping table if
// it is missing — so asking it "what version are you at?" is a write. A second
// role asking that question while the controller migrates races it on the
// CREATE, and one of the two loses with "table goose_db_version already
// exists". The controller is usually the loser, because it is doing more work,
// which turns a harmless status check by a worker into a failed startup for the
// role that owns the schema.
//
// So this reads, on the reader pool, and reports 0 for a database that has
// never been migrated. Roles that do not own the schema (§7, ADR-0003) must use
// this one.
func AppliedSchemaVersion(ctx context.Context, db *DB) (int64, error) {
	var exists int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version'`).
		Scan(&exists); err != nil {
		return 0, fmt.Errorf("sqlite: looking for the migration table: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}

	// Mirror goose's own ordering: the most recently recorded applied row wins,
	// so a rollback is reflected rather than averaged away by a max().
	rows, err := db.Reader().QueryContext(ctx,
		`SELECT version_id, is_applied FROM goose_db_version ORDER BY id DESC`)
	if err != nil {
		return 0, fmt.Errorf("sqlite: reading the applied schema version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			version int64
			applied bool
		)
		if err := rows.Scan(&version, &applied); err != nil {
			return 0, fmt.Errorf("sqlite: reading the applied schema version: %w", err)
		}
		if applied {
			return version, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: reading the applied schema version: %w", err)
	}
	return 0, nil
}
