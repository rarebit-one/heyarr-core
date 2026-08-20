package events

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

func newLogWithDB(t *testing.T) (*Log, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	l, err := New(Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	return l, db
}

// The reason EmitTx exists. The control plane is single-writer (ADR-0003): the
// writer pool holds one connection and a write transaction takes the database's
// write lock up front (_txlock=immediate). An Emit nested inside a transaction
// therefore waits — for the connection its own caller is holding, or failing
// that for the lock its own caller is holding. Either way the symptom is a hang
// until the context expires rather than an error, which is why this is asserted
// rather than left as a comment.
func TestEmitInsideATransactionBlocksOnTheWriteLockItsCallerHolds(t *testing.T) {
	l, db := newLogWithDB(t)

	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()

	err := db.InTx(ctx, func(*sql.Tx) error {
		_, emitErr := l.Emit(ctx, TypeBlobCreated, "blob", "b1", nil)
		return emitErr
	})
	if err == nil {
		t.Fatal("Emit inside a transaction succeeded — the control plane is no longer single-writer, " +
			"which breaks ADR-0003; if that was deliberate, EmitTx's contract needs revisiting")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error from the blocked connection, got %v", err)
	}
}

func TestEmitTxIsInvisibleUntilTheTransactionCommits(t *testing.T) {
	l, db := newLogWithDB(t)

	var emitted Event
	err := db.InTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		emitted, err = l.EmitTx(t.Context(), tx, TypeIngestCompleted, "asset", "a1", map[string]bool{"deduplicated": false})
		if err != nil {
			return err
		}
		if emitted.Seq == 0 {
			t.Error("EmitTx did not assign a sequence")
		}
		// Readers use a separate pool; WAL means they see the last committed
		// state, so nothing yet.
		got, err := l.Since(t.Context(), 0, nil, 10)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Errorf("an uncommitted event was already visible to readers: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	got, err := l.Since(t.Context(), 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != emitted.ID {
		t.Fatalf("after commit want the one emitted event, got %+v", got)
	}
}

func TestEmitTxLeavesNoEventBehindWhenTheTransactionRollsBack(t *testing.T) {
	l, db := newLogWithDB(t)

	sentinel := errors.New("sentinel")
	err := db.InTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := l.EmitTx(t.Context(), tx, TypeBlobCreated, "blob", "b1", nil); err != nil {
			return err
		}
		if _, err := l.EmitTx(t.Context(), tx, TypeAssetCreated, "asset", "a1", nil); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the sentinel back, got %v", err)
	}

	got, err := l.Since(t.Context(), 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a rolled-back transaction left %d events behind: %+v", len(got), got)
	}
}

// A subscriber must never see an event whose transaction later rolls back: it
// may act on it, and the log is the record of what happened (§76).
func TestEmitTxDoesNotFanOutUntilPublish(t *testing.T) {
	l, db := newLogWithDB(t)
	sub := l.Subscribe(4)
	defer sub.Close()

	var emitted Event
	if err := db.InTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		emitted, err = l.EmitTx(t.Context(), tx, TypeBlobCreated, "blob", "b1", nil)
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}

	select {
	case e := <-sub.Events():
		t.Fatalf("EmitTx fanned out on its own: %+v", e)
	default:
	}

	l.Publish(emitted)
	select {
	case e := <-sub.Events():
		if e.ID != emitted.ID {
			t.Fatalf("Publish delivered %s, want %s", e.ID, emitted.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish delivered nothing")
	}
}

func TestEmitTxRequiresATransaction(t *testing.T) {
	l, _ := newLogWithDB(t)
	if _, err := l.EmitTx(t.Context(), nil, TypeBlobCreated, "blob", "b1", nil); err == nil {
		t.Fatal("EmitTx accepted a nil transaction")
	}
}
