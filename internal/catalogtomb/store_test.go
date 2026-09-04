package catalogtomb_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

var now = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

type fixture struct {
	store *catalogtomb.Store
	db    *sqlite.DB
	clock *fixedClock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{t: now}
	store, err := catalogtomb.New(catalogtomb.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: store, db: db, clock: clock}
}

// insertWork adds a live work row the way the scanner's get-or-create would,
// returning its id. Enough of 00002_core.sql's NOT NULLs to be a valid row.
func (f *fixture) insertWork(t *testing.T, contentType, workKey, title string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	ts := now.Format(time.RFC3339Nano)
	_, err := f.db.Writer().ExecContext(context.Background(),
		`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, NULL, '{}', ?, ?)`,
		id, contentType, workKey, title, title, ts, ts)
	if err != nil {
		t.Fatalf("insert work: %v", err)
	}
	return id
}

func (f *fixture) workExists(t *testing.T, contentType, workKey string) bool {
	t.Helper()
	var one int
	err := f.db.Reader().QueryRowContext(context.Background(),
		`SELECT 1 FROM works WHERE content_type = ? AND work_key = ?`, contentType, workKey).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("work exists: %v", err)
	}
	return true
}

func mustSign(t *testing.T, priv ed25519.PrivateKey, kind catalogop.Kind, ct, wk string, prev []string, at time.Time) string {
	t.Helper()
	tok, err := catalogop.Sign(priv, kind, ct, wk, prev, at)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// TestDeleteRecordSuppressesWork: recording a delete op materialises the
// tombstone AND removes the live work row — the remove-wins application. This
// is a delete learned from a peer converging at this site.
func TestDeleteRecordSuppressesWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	peer := newKey(t)

	f.insertWork(t, "movie", "the-thing-1982", "The Thing")
	if !f.workExists(t, "movie", "the-thing-1982") {
		t.Fatal("precondition: work should exist")
	}

	del := mustSign(t, peer, catalogop.OpDelete, "movie", "the-thing-1982", nil, now)
	if err := f.store.RecordOps(ctx, []string{del}); err != nil {
		t.Fatalf("record delete: %v", err)
	}

	if f.workExists(t, "movie", "the-thing-1982") {
		t.Error("the work row survived a recorded delete")
	}
	tombstoned, err := f.store.Tombstoned(ctx, "movie", "the-thing-1982")
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Error("the work is not tombstoned in the materialised view")
	}
}

// TestRecordIsIdempotent: recording the same op twice is one row and no error —
// the property a peer sync relies on to re-push freely.
func TestRecordIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	peer := newKey(t)

	del := mustSign(t, peer, catalogop.OpDelete, "movie", "alien-1979", nil, now)
	if err := f.store.RecordOps(ctx, []string{del}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordOps(ctx, []string{del, del}); err != nil {
		t.Fatal(err)
	}
	ops, err := f.store.Ops(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Errorf("recorded %d ops for one op learned three times, want 1", len(ops))
	}
}

// TestMalformedOpIsRefused: a token that does not verify is refused whole and
// nothing is written.
func TestMalformedOpIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.RecordOps(ctx, []string{"not-a-token"}); err == nil {
		t.Error("expected a malformed op to be refused")
	}
	ops, err := f.store.Ops(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Errorf("a refused batch wrote %d ops", len(ops))
	}
}

// TestPartitionMergeConvergesAtStore is the two-site story at the storage layer:
// site A holds a work live and records site B's delete only after the partition
// heals; once it does, A's work is suppressed and tombstoned — the same state B
// reached when it authored the delete.
func TestPartitionMergeConvergesAtStore(t *testing.T) {
	ctx := context.Background()
	siteBKey := newKey(t)

	// Site B authors a delete (its own works row, if any, would be gone there).
	del := mustSign(t, siteBKey, catalogop.OpDelete, "series", "the-wire", nil, now)

	// Site A: the work is still live here through the partition.
	a := newFixture(t)
	a.insertWork(t, "series", "the-wire", "The Wire")

	// The partition heals: A learns B's op (via a peer sync -> RecordOps).
	if err := a.store.RecordOps(ctx, []string{del}); err != nil {
		t.Fatalf("site A recording peer delete: %v", err)
	}

	if a.workExists(t, "series", "the-wire") {
		t.Error("site A did not converge — the deleted work is still present")
	}
	tombstoned, err := a.store.Tombstoned(ctx, "series", "the-wire")
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Error("site A did not tombstone the peer-deleted work")
	}
}

// TestCausalRestoreLiftsMaterialisedTombstone: a restore that cites the delete
// removes the materialised tombstone, so the scanner may re-derive the work
// again (the re-ripped-later case).
func TestCausalRestoreLiftsMaterialisedTombstone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	peer := newKey(t)

	del := mustSign(t, peer, catalogop.OpDelete, "movie", "the-thing-1982", nil, now)
	if err := f.store.RecordOps(ctx, []string{del}); err != nil {
		t.Fatal(err)
	}
	tombstoned, err := f.store.Tombstoned(ctx, "movie", "the-thing-1982")
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Fatal("precondition: work should be tombstoned after delete")
	}

	restore := mustSign(t, peer, catalogop.OpRestore, "movie", "the-thing-1982",
		[]string{catalogop.Hash(del)}, now.Add(time.Hour))
	if err := f.store.RecordOps(ctx, []string{restore}); err != nil {
		t.Fatal(err)
	}
	tombstoned, err = f.store.Tombstoned(ctx, "movie", "the-thing-1982")
	if err != nil {
		t.Fatal(err)
	}
	if tombstoned {
		t.Error("a causal restore did not lift the materialised tombstone")
	}
}

// TestConcurrentRestoreDoesNotLift: a restore NOT citing the delete leaves the
// tombstone in place at the storage layer too — remove-wins end to end.
func TestConcurrentRestoreDoesNotLift(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := newKey(t)
	b := newKey(t)

	del := mustSign(t, a, catalogop.OpDelete, "movie", "the-thing-1982", nil, now)
	concurrentRestore := mustSign(t, b, catalogop.OpRestore, "movie", "the-thing-1982", nil, now.Add(time.Minute))
	if err := f.store.RecordOps(ctx, []string{del, concurrentRestore}); err != nil {
		t.Fatal(err)
	}
	tombstoned, err := f.store.Tombstoned(ctx, "movie", "the-thing-1982")
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Error("a concurrent restore lifted the tombstone — remove-wins violated at the store")
	}
}
