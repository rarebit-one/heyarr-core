package sqlite

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

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
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("sqlite: selecting migration dialect: %w", err)
	}

	known, err := maxKnownVersion()
	if err != nil {
		return err
	}

	// goose creates its bookkeeping table on first use, so a database with no
	// version yet is version 0 rather than an error.
	current, err := goose.GetDBVersionContext(ctx, db.Writer())
	if err != nil {
		return fmt.Errorf("sqlite: reading schema version: %w", err)
	}
	if current > known {
		return fmt.Errorf("%w: database is at version %d, this binary knows up to %d — "+
			"it was migrated by a newer build of heyarr; upgrade rather than downgrade, "+
			"because an older binary writing against a newer schema corrupts it silently",
			ErrSchemaNewerThanBinary, current, known)
	}

	if err := goose.UpContext(ctx, db.Writer(), "migrations"); err != nil {
		return fmt.Errorf("sqlite: applying migrations: %w", err)
	}

	applied, err := goose.GetDBVersionContext(ctx, db.Writer())
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
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("sqlite: selecting migration dialect: %w", err)
	}
	if err := goose.DownContext(ctx, db.Writer(), "migrations"); err != nil {
		return fmt.Errorf("sqlite: rolling back migration: %w", err)
	}
	return nil
}

// SchemaVersion reports the version currently applied to the database.
func SchemaVersion(ctx context.Context, db *DB) (int64, error) {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, db.Writer())
}

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
