package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// The pragmas are the correctness foundation for everything above them, and a
// DSN typo yields a working database with foreign keys silently off.
func TestOpenAppliesPragmasToBothPools(t *testing.T) {
	db := openTestDB(t)
	for name, pool := range map[string]*sql.DB{"writer": db.Writer(), "reader": db.Reader()} {
		var journal, fks string
		if err := pool.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow("PRAGMA foreign_keys").Scan(&fks); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" {
			t.Errorf("%s pool journal_mode = %q, want wal", name, journal)
		}
		if fks != "1" {
			t.Errorf("%s pool foreign_keys = %q, want on", name, fks)
		}
	}
}

// Foreign keys being on is worth asserting behaviourally, not just by pragma —
// the pragma can read as on while the constraint is unenforced if the schema
// was created without it.
func TestForeignKeysAreActuallyEnforced(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY) STRICT`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TABLE child (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER NOT NULL REFERENCES parent(id)
		) STRICT`)
		return err
	})
	if err != nil {
		t.Fatalf("creating tables: %v", err)
	}
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO child (id, parent_id) VALUES (1, 999)`)
		return err
	})
	if err == nil {
		t.Fatal("inserting a row referencing a nonexistent parent succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %q, want a foreign key violation", err)
	}
}

func TestOpenRejectsInMemory(t *testing.T) {
	_, err := Open(t.Context(), Options{Path: ":memory:"})
	if err == nil {
		t.Fatal("Open accepted :memory:")
	}
	// The reader and writer pools would get separate private databases, which
	// is a confusing failure worth naming rather than discovering.
	if !strings.Contains(err.Error(), "different databases") {
		t.Errorf("error = %q, want it to explain why", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(t.Context(), Options{}); err == nil {
		t.Error("Open accepted an empty path")
	}
}

// The acceptance criterion for M1-03: concurrent writers must never leak
// SQLITE_BUSY to a caller. The single-writer pool turns contention into a queue
// in Go rather than an error from SQLite.
func TestConcurrentWritersNeverLeakBusy(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	const writers, each = 8, 40
	var wg sync.WaitGroup
	errCh := make(chan error, writers*each)
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range each {
				id := w*each + i
				err := db.InTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.Exec(`INSERT INTO counter (id, n) VALUES (?, ?)`, id, i)
					return err
				})
				if err != nil {
					errCh <- err
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent write failed: %v", err)
	}

	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM counter`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := writers * each; n != want {
		t.Errorf("wrote %d rows, want %d", n, want)
	}
}

// WAL's point: a read must not block behind a write in flight.
func TestReadsProceedDuringAWrite(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	inTx := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
				return err
			}
			close(inTx)
			<-release
			return nil
		})
	}()

	<-inTx
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n int
	if err := db.Reader().QueryRowContext(readCtx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		close(release)
		t.Fatalf("read blocked behind an open write transaction: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("nope")
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx returned %v, want the callback's error", err)
	}

	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows survived a rolled-back transaction, want 0", n)
	}
}

// A panic must roll back and then keep travelling. Swallowing it would leave
// the caller believing the work committed.
func TestInTxRollsBackAndRepanicsOnPanic(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("InTx swallowed the panic")
			}
		}()
		_ = db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows survived a panicking transaction, want 0", n)
	}
}

// §50 replicates controller backups to peers, so after shutdown the database
// file must stand alone — a populated -wal beside a copied file is a silently
// stale backup.
//
// Note this passes whether or not Close checkpoints explicitly, because SQLite
// removes the WAL itself on last-connection close. It asserts the property, not
// our implementation of it; the property is what backups depend on.
func TestDatabaseFileIsSelfContainedAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.db")
	db, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path + "-wal")
	if err == nil && info.Size() > 0 {
		t.Errorf("WAL is %d bytes after Close, want it checkpointed away", info.Size())
	}
}
