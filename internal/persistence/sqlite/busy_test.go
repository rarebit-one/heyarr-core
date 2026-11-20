package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holdWriteLock opens a second, independent connection to path and takes the
// write lock on it, the way a server process holds it while a CLI command runs
// beside it (#910). The returned func releases it.
func holdWriteLock(t *testing.T, path string) func() {
	t.Helper()
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("taking the write lock on the other connection: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
			t.Errorf("releasing the write lock: %v", err)
		}
		_ = conn.Close()
	}
	t.Cleanup(release)
	return release
}

// waitSignal is a slog handler that closes a channel the first time the
// database logs that it is waiting for another process's lock, so a test can
// release that lock exactly when contention has been observed rather than
// after a sleep.
type waitSignal struct {
	seen chan struct{}
	once bool
}

func (w *waitSignal) Enabled(context.Context, slog.Level) bool { return true }
func (w *waitSignal) Handle(_ context.Context, r slog.Record) error {
	if !w.once && strings.Contains(r.Message, "write lock") {
		w.once = true
		close(w.seen)
	}
	return nil
}
func (w *waitSignal) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *waitSignal) WithGroup(string) slog.Handler      { return w }

func openContended(t *testing.T, path string, writeWait time.Duration, h slog.Handler) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{
		Path: path, BusyTimeout: 20 * time.Millisecond, WriteWait: writeWait, Logger: slog.New(h),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// #910: `heyarr gc --apply` beside a running server died with "beginning
// transaction: database is locked (SQLITE_BUSY)" when a write in the other
// process outlasted busy_timeout. A second connection holding a write
// transaction for longer than busy_timeout must make InTx wait, not fail.
func TestInTxWaitsOutAnotherProcesssWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heyarr.db")
	signal := &waitSignal{seen: make(chan struct{})}
	db := openContended(t, path, time.Minute, signal)
	if err := db.InTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE n (v INTEGER NOT NULL) STRICT`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	release := holdWriteLock(t, path)
	done := make(chan error, 1)
	go func() {
		done <- db.InTx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO n (v) VALUES (1)`)
			return err
		})
	}()

	// Hold the lock until InTx has demonstrably hit SQLITE_BUSY at least once
	// (busy_timeout is 20ms, so it has), then let go.
	select {
	case <-signal.seen:
	case err := <-done:
		t.Fatalf("InTx returned (%v) while another connection held the write lock", err)
	case <-time.After(30 * time.Second):
		t.Fatal("InTx never reported waiting for the lock")
	}
	release()

	if err := <-done; err != nil {
		t.Fatalf("InTx failed after the other process released its lock: %v", err)
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM n`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d after the contended write, want 1", n)
	}
}

// The wait is bounded: a lock that is never released is an error naming the
// cause, not a hang, and fn is never run.
func TestInTxGivesUpOnALockThatIsNeverReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heyarr.db")
	db := openContended(t, path, 200*time.Millisecond, slog.DiscardHandler)
	holdWriteLock(t, path)

	ran := false
	err := db.InTx(t.Context(), func(*sql.Tx) error { ran = true; return nil })
	if err == nil {
		t.Fatal("InTx succeeded while another connection held the write lock throughout")
	}
	if !IsBusy(err) {
		t.Errorf("error = %v, want it to still be SQLITE_BUSY underneath", err)
	}
	if !strings.Contains(err.Error(), "another process held the write lock") {
		t.Errorf("error = %v, want it to say why it gave up", err)
	}
	if ran {
		t.Error("fn ran without a transaction")
	}
}

// Cancelling the caller stops the wait.
func TestInTxStopsWaitingWhenCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heyarr.db")
	signal := &waitSignal{seen: make(chan struct{})}
	db := openContended(t, path, time.Hour, signal)
	holdWriteLock(t, path)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- db.InTx(ctx, func(*sql.Tx) error { return nil }) }()
	<-signal.seen
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("InTx kept waiting after its context was cancelled")
	}
}

// Only SQLITE_BUSY is waited out. Anything else from BEGIN is returned at
// once, and IsBusy says no.
func TestIsBusyOnlyMatchesLocking(t *testing.T) {
	db := openTestDB(t)
	_, err := db.Writer().Exec(`SELECT * FROM no_such_table`)
	if err == nil {
		t.Fatal("querying a missing table succeeded")
	}
	if IsBusy(err) {
		t.Errorf("IsBusy(%v) = true, want false", err)
	}
	if IsBusy(nil) {
		t.Error("IsBusy(nil) = true")
	}
}
