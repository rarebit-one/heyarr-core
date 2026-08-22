package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; see ADR-0004
)

// ErrNoSnapshot is a peer that has never built one.
//
// It is a distinct error rather than an empty result, and that distinction is
// the acceptance condition this package exists to hold up. In Milestone 7,
// "the snapshot contains no works" means THE LIBRARY IS EMPTY and "there is no
// snapshot" means I CANNOT HELP YOU. Those are different sentences, they lead
// to different behaviour, and a design that returns a zero value for both is a
// design that will one day tell a user their library is empty during an
// outage.
var ErrNoSnapshot = errors.New("catalog: this peer has no catalog snapshot")

// ErrStaleSnapshot is an apply whose version does not advance.
//
// Monotonicity is refused here as well as allocated at the controller because
// the two failures are different. The controller guarantees it issues
// increasing versions; this guarantees the peer never MOVES BACKWARDS — which
// is what a replayed response, a retried request or a snapshot restored from
// an older backup would otherwise do, silently, leaving a peer confidently
// serving last week's catalogue.
var ErrStaleSnapshot = errors.New("catalog: the snapshot version does not advance")

// ErrReadOnly is a write attempted through a read handle at the Go layer.
//
// It is belt to the storage layer's braces. The braces are the real mechanism
// — a read handle is opened mode=ro and refuses writes inside SQLite — and the
// acceptance assertion is against THAT, because a Go-level guard is only ever
// as good as the last person who added a method.
var ErrReadOnly = errors.New("catalog: this snapshot handle is read-only")

// errControlDatabase is the snapshot builder pointed at a control database.
var errControlDatabase = errors.New("catalog: refusing to use a control database as a snapshot store")

// busyTimeout bounds how long an open waits for a lock.
//
// There is only ever one writer here, so contention should not happen. The
// timeout exists for the case that does: a read handle opening while a build
// is committing.
const busyTimeout = 5 * time.Second

// Store is a peer's materialised catalog snapshot: one SQLite file, one
// writer, and read handles that cannot write.
//
// # Why a separate file
//
// §52: "The snapshot should not be treated as independently writable control
// state." A shadow schema inside the peer's control database would satisfy
// that sentence only by convention, and convention is exactly what fails under
// deadline. A separate file makes it structural — there is no statement anyone
// can write against this handle that reaches the control plane, and no
// statement against the control plane that reaches this. Invariant 5 and
// ADR-0003 are why that is worth a file rather than a code review.
type Store struct {
	db       *sql.DB
	path     string
	writable bool
}

// Options configure a writable snapshot store.
type Options struct {
	// Path to the snapshot database file. It is created if absent.
	Path string
}

// SnapshotPathFor returns the conventional snapshot location beneath a peer's
// data directory.
//
// It is deliberately NOT sqlite.DataDirFor's heyarr.db with a different
// extension: the two files must be obviously different things in a directory
// listing, because the whole design rests on nobody confusing them.
func SnapshotPathFor(dataDir string) string {
	return filepath.Join(dataDir, "catalog-snapshot.db")
}

// Open prepares the snapshot store for the ONE writer that is allowed: the
// snapshot builder.
//
// Nothing else in the system may call this. Read paths call [OpenReadOnly],
// which is the enforcement rather than the etiquette — a handle from there
// cannot write even if a future caller decides it should be able to.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("catalog: a snapshot store needs a path")
	}
	if opts.Path == ":memory:" {
		return nil, errors.New("catalog: :memory: is not a snapshot store — " +
			"a snapshot that vanishes on restart is worse than none, because the peer " +
			"believes it has one until the moment it needs it")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("catalog: preparing the snapshot directory: %w", err)
	}

	db, err := sql.Open("sqlite", writerDSN(opts.Path))
	if err != nil {
		return nil, fmt.Errorf("catalog: opening the snapshot store: %w", err)
	}
	// One connection. There is exactly one writer by design, and a pool would
	// be a pool of writers against a database whose entire premise is that it
	// has one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: opts.Path, writable: true}
	if err := s.refuseControlDatabase(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("catalog: preparing the snapshot schema: %w", err)
	}
	return s, nil
}

// OpenReadOnly opens an existing snapshot for reading, and only for reading.
//
// The refusal is SQLite's, not this package's: mode=ro opens the file without
// write access and query_only refuses a write statement outright, so an
// attempted UPDATE fails with SQLITE_READONLY at the storage layer. That
// matters more than it sounds. A Go-level guard proves that no code currently
// writes; this proves that no code CAN — which is the form §52's constraint
// has to take if it is to survive the next person in a hurry.
//
// A file that does not exist is [ErrNoSnapshot], not an empty store. See that
// error for why the distinction is load-bearing.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("catalog: a snapshot store needs a path")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: nothing at %s", ErrNoSnapshot, path)
		}
		return nil, fmt.Errorf("catalog: looking for the snapshot store: %w", err)
	}
	db, err := sql.Open("sqlite", readerDSN(path))
	if err != nil {
		return nil, fmt.Errorf("catalog: opening the snapshot store read-only: %w", err)
	}
	s := &Store{db: db, path: path, writable: false}
	if err := s.assertReadOnly(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// writerDSN is the builder's connection string.
//
// journal_mode is DELETE rather than WAL, which is the opposite of the
// controller's choice and deliberate. WAL buys concurrent readers during a
// write; there is nothing to buy here, because a build is one transaction and
// readers of a half-built snapshot are not wanted. What WAL would cost is
// real: a read-only connection to a WAL database needs to create a -shm file,
// so "open the snapshot read-only" would need write access to the directory —
// and the mechanism this package rests on would depend on a filesystem
// permission rather than on SQLite.
func writerDSN(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(DELETE)"+
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)&_txlock=immediate",
		path, busyTimeout.Milliseconds())
}

// readerDSN is the read path's connection string, and the mechanism §52 asks
// for. mode=ro opens the file without write access; query_only refuses a write
// statement before it reaches the file at all. Both, because they fail at
// different layers and a caller should not have to know which one caught it.
func readerDSN(path string) string {
	return fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(true)"+
		"&_pragma=foreign_keys(ON)", path, busyTimeout.Milliseconds())
}

// assertReadOnly verifies the read handle really is one.
//
// A DSN typo yields a working database with query_only silently off, which is
// the same class of failure the controller's verifyPragmas exists for — except
// that here the pragma IS the security property rather than a correctness aid.
// Checking it at open is what keeps "the read path cannot write" from being a
// claim about a connection string.
func (s *Store) assertReadOnly(ctx context.Context) error {
	var queryOnly int
	if err := s.db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return fmt.Errorf("catalog: verifying the snapshot handle is read-only: %w", err)
	}
	if queryOnly != 1 {
		return errors.New("catalog: the snapshot handle is not query_only — " +
			"the connection string did not take effect, and a read path that can write " +
			"is the one thing this store exists to prevent (§52)")
	}
	return nil
}

// refuseControlDatabase stops the builder writing into the control plane.
//
// The cheapest way to end up with two writers against the control database —
// which is the failure Invariant 5 and ADR-0003 rank as the most expensive in
// this system — is a misconfigured path. A control database is recognisable:
// goose keeps its bookkeeping table there and nothing else does. Refusing here
// costs one query at startup and removes a whole category of catastrophe.
func (s *Store) refuseControlDatabase(ctx context.Context) error {
	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('goose_db_version', 'peers')`).
		Scan(&found)
	if err != nil {
		return fmt.Errorf("catalog: inspecting the snapshot store: %w", err)
	}
	if found > 0 {
		return fmt.Errorf("%w: %s holds control-plane tables. The snapshot is a separate "+
			"database with one writer (§52, Invariant 5, ADR-0003); pointing the snapshot "+
			"builder at the control database would make it a second one", errControlDatabase, s.path)
	}
	return nil
}

// Path is the snapshot file's location.
func (s *Store) Path() string { return s.path }

// Writable reports whether this handle is the builder's.
func (s *Store) Writable() bool { return s.writable }

// Close releases the handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Query runs a read against the snapshot.
//
// This is the seam Milestone 7 will build its degraded read path on. It is
// available on both kinds of handle, because reading is the only thing a
// snapshot is for.
func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...) //nolint:sqlclosecheck // the caller owns the rows
}

// Exec runs a statement against the snapshot.
//
// It exists on the read handle ON PURPOSE, and it is the assertion point for
// §52. Hiding it behind a type that has no Exec method would make the
// constraint a fact about this package's API surface — true until somebody
// adds a method — whereas leaving it here and letting SQLite refuse makes it a
// fact about the file. A read handle returns SQLITE_READONLY from the storage
// layer; the Go-level [ErrReadOnly] below is only there so the message names
// the reason.
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil && !s.writable {
		return nil, fmt.Errorf("%w (%s): %w", ErrReadOnly, s.path, err)
	}
	return res, err
}

// Metadata reports what this snapshot is a fact ABOUT: which controller, which
// version, and when.
//
// [ErrNoSnapshot] when the store has never had one applied. Not a zero Meta —
// see that error.
func (s *Store) Metadata(ctx context.Context) (Meta, error) {
	var (
		m         Meta
		generated string
		watermark string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT controller_id, version, generated_at, kind, watermark FROM snapshot_meta WHERE id = 1`).
		Scan(&m.ControllerID, &m.Version, &generated, &m.Kind, &watermark)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Meta{}, fmt.Errorf("%w: %s has never had one applied", ErrNoSnapshot, s.path)
	case err != nil:
		// A store whose schema was never created answers the same way a store
		// with no row does: the peer has no snapshot. Anything else would make
		// "never built" depend on whether the file had been touched.
		if isMissingTable(err) {
			return Meta{}, fmt.Errorf("%w: %s has no snapshot schema", ErrNoSnapshot, s.path)
		}
		return Meta{}, fmt.Errorf("catalog: reading snapshot metadata: %w", err)
	}
	if m.GeneratedAt, err = parseStamp(generated); err != nil {
		return Meta{}, err
	}
	if m.Watermark, err = parseStamp(watermark); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// StoredDigest is the content digest recorded when the snapshot was applied.
func (s *Store) StoredDigest(ctx context.Context) (string, error) {
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT content_digest FROM snapshot_meta WHERE id = 1`).Scan(&digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %s has never had one applied", ErrNoSnapshot, s.path)
	case err != nil && isMissingTable(err):
		return "", fmt.Errorf("%w: %s has no snapshot schema", ErrNoSnapshot, s.path)
	case err != nil:
		return "", fmt.Errorf("catalog: reading the snapshot digest: %w", err)
	}
	return digest, nil
}

// isMissingTable recognises a store whose schema has never been created.
//
// It matches on the driver's message rather than a code because
// modernc.org/sqlite reports this one as a generic error, and the alternative
// — treating every failure as "no snapshot" — would turn a corrupt file into a
// cheerful "this peer has never built one".
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table")
}

func parseStamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("catalog: %q is not a timestamp: %w", s, err)
	}
	return t.UTC(), nil
}

func formatStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
