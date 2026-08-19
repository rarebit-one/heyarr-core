// Package sqlite owns the controller database: connection policy, pragmas,
// migrations and transaction helpers (spec §49).
//
// The control plane is single-writer by design (ADR-0003). Resilience comes
// from backup streams to Full Peers (§50) and restore tooling (§51) — never
// from replicating a live SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; see ADR-0004
)

// DB is the controller database. It deliberately exposes two pools:
//
//	Writer — exactly one connection. SQLite permits a single writer, and
//	         funnelling writes through one connection means contention is
//	         resolved in Go, where it is a queue, rather than in SQLite, where
//	         it surfaces as SQLITE_BUSY to whichever caller lost.
//	Reader — many connections. WAL lets readers proceed while a write is in
//	         flight, so read concurrency costs nothing.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	path   string
	log    *slog.Logger
}

// Options configure the connection policy.
type Options struct {
	// Path to the database file. The special value ":memory:" is rejected —
	// see Open.
	Path string
	// ReadPoolSize bounds the reader pool. Zero means a sensible default.
	ReadPoolSize int
	// BusyTimeout is how long SQLite waits for a lock before returning
	// SQLITE_BUSY. Zero means a sensible default.
	BusyTimeout time.Duration
	Logger      *slog.Logger
}

const (
	defaultReadPoolSize = 8
	defaultBusyTimeout  = 5 * time.Second
)

// Open prepares both pools and verifies the pragmas actually took effect.
//
// Pragmas are applied through the DSN rather than by issuing PRAGMA statements
// after connecting, because a pooled connection that is closed and reopened
// would otherwise silently lose them — every connection in the pool must carry
// the same settings, and the DSN is the only place that is guaranteed.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Path == "" {
		return nil, errors.New("sqlite: path must be set")
	}
	// An in-memory database would give each pool its own private database and
	// silently lose everything on restart. Both are catastrophic and neither is
	// obvious from a stack trace, so refuse rather than surprise.
	if opts.Path == ":memory:" {
		return nil, errors.New("sqlite: :memory: is not supported — the reader and writer pools would " +
			"see different databases; use a file under a temporary directory instead")
	}
	if opts.ReadPoolSize <= 0 {
		opts.ReadPoolSize = defaultReadPoolSize
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = defaultBusyTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	// Converting a fresh database to WAL takes a brief exclusive lock, and it is
	// a one-time database-level transition — once done, every later connection
	// setting journal_mode=WAL is a no-op needing no lock. So do it once, up
	// front, with a retry, rather than letting every pool connection race for
	// it and letting one lose with a baffling SQLITE_BUSY at startup.
	if err := ensureWALMode(ctx, opts); err != nil {
		return nil, err
	}

	writer, err := openPool(dsn(opts, true), 1)
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening writer: %w", err)
	}
	reader, err := openPool(dsn(opts, false), opts.ReadPoolSize)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("sqlite: opening reader: %w", err)
	}

	db := &DB{writer: writer, reader: reader, path: opts.Path, log: opts.Logger}
	if err := db.verifyPragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ensureWALMode performs the one-time journal-mode transition on a single
// connection, retrying while the database is locked by another process doing
// the same thing.
func ensureWALMode(ctx context.Context, opts Options) error {
	conn, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)",
		opts.Path, opts.BusyTimeout.Milliseconds()))
	if err != nil {
		return fmt.Errorf("sqlite: opening %s: %w", opts.Path, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	deadline := time.Now().Add(opts.BusyTimeout)
	backoff := 2 * time.Millisecond
	for attempt := 1; ; attempt++ {
		var mode string
		err = conn.QueryRowContext(ctx, "PRAGMA journal_mode(WAL)").Scan(&mode)
		if err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("journal_mode is %q after requesting WAL", mode)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("sqlite: enabling WAL on %s: %w", opts.Path, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sqlite: enabling WAL on %s after %d attempts: %w", opts.Path, attempt, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sqlite: enabling WAL on %s: %w", opts.Path, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < 100*time.Millisecond {
			backoff *= 2
		}
	}
}

func dsn(opts Options, writer bool) string {
	ms := opts.BusyTimeout.Milliseconds()
	// _txlock=immediate makes a write transaction take its lock up front rather
	// than upgrading mid-transaction, which is where SQLite's deadlock-shaped
	// SQLITE_BUSY comes from — the upgrade cannot wait, so it fails instantly
	// regardless of busy_timeout.
	// busy_timeout MUST come first. Pragmas are applied in order, and
	// journal_mode=WAL needs a brief exclusive lock — so if busy_timeout is set
	// after it, the one statement that actually contends is the one running
	// with no timeout at all. Two roles starting together, or even this
	// process's own reader and writer pools, then race and one fails to open
	// with SQLITE_BUSY.
	d := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", opts.Path, ms)
	if writer {
		d += "&_txlock=immediate"
	}
	return d
}

func openPool(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// SQLite connections are cheap and carry per-connection pragma state;
	// recycling them buys nothing and risks losing that state.
	db.SetConnMaxLifetime(0)
	return db, nil
}

// verifyPragmas asserts the settings the correctness of everything else depends
// on. A DSN typo yields a working database with foreign keys silently off,
// which is exactly the class of failure that surfaces months later as orphaned
// rows nobody can explain.
func (db *DB) verifyPragmas(ctx context.Context) error {
	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
	}
	for _, pool := range []struct {
		name string
		sql  *sql.DB
	}{{"writer", db.writer}, {"reader", db.reader}} {
		for _, c := range checks {
			var got string
			if err := pool.sql.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
				return fmt.Errorf("sqlite: reading %s on the %s pool: %w", c.pragma, pool.name, err)
			}
			if got != c.want {
				return fmt.Errorf("sqlite: %s is %q on the %s pool, want %q — "+
					"the connection string did not take effect", c.pragma, got, pool.name, c.want)
			}
		}
	}
	return nil
}

// Writer returns the single-writer pool. Every statement that mutates state
// goes through it.
func (db *DB) Writer() *sql.DB { return db.writer }

// Reader returns the read pool. Reads proceed concurrently with a write in
// flight, which is what WAL buys.
func (db *DB) Reader() *sql.DB { return db.reader }

// Path is the database file's location.
func (db *DB) Path() string { return db.path }

// Close shuts both pools down, leaving the database file self-contained.
//
// The explicit checkpoint is belt-and-braces: SQLite also checkpoints and
// removes the WAL when the last connection closes, so on this path it is
// redundant today. It is kept because it stops being redundant the moment
// anything holds a connection open past Close — and because §50 replicates
// controller backups to peers, where a populated -wal beside a copied database
// file is a silently stale backup rather than a loud failure.
func (db *DB) Close() error {
	var errs []error
	if db.writer != nil {
		if _, err := db.writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			errs = append(errs, fmt.Errorf("sqlite: checkpointing WAL: %w", err))
		}
		if err := db.writer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("sqlite: closing writer: %w", err))
		}
	}
	if db.reader != nil {
		if err := db.reader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("sqlite: closing reader: %w", err))
		}
	}
	return errors.Join(errs...)
}

// InTx runs fn inside a write transaction, committing on success and rolling
// back on error or panic. A panic is re-raised after the rollback: swallowing
// it would leave the caller believing the work succeeded.
func (db *DB) InTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: beginning transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("sqlite: rolling back: %w", rbErr))
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: committing: %w", err)
	}
	return nil
}

// DataDirFor returns the conventional database path beneath a data directory.
func DataDirFor(dataDir string) string { return filepath.Join(dataDir, "heyarr.db") }
