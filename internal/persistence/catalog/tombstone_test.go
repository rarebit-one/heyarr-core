package catalog_test

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Catalog.Tombstoned reads the materialised work_tombstones view — the read the
// scan pipeline and resolveWork both use to suppress re-materialising a work a
// sibling deleted (ADR-0073, #449). A recorded delete op tombstones its target
// and only its target.
func TestCatalogTombstonedReflectsARecordedDeleteOp(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := fixedClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := catalogtomb.New(catalogtomb.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing tombstoned yet.
	if got, err := cat.Tombstoned(ctx, "movie", "arrival|2016"); err != nil || got {
		t.Fatalf("before any op: tombstoned=%v err=%v, want false", got, err)
	}

	// Record a delete op for one work.
	_, signer, _ := ed25519.GenerateKey(nil)
	tok, err := catalogop.Sign(signer, catalogop.OpDelete, "movie", "arrival|2016", nil, clock.t)
	if err != nil {
		t.Fatal(err)
	}
	if err := tomb.RecordOps(ctx, []string{tok}); err != nil {
		t.Fatal(err)
	}

	// The named work is now tombstoned; a different key is not.
	if got, err := cat.Tombstoned(ctx, "movie", "arrival|2016"); err != nil || !got {
		t.Fatalf("after the delete op: tombstoned=%v err=%v, want true", got, err)
	}
	if got, err := cat.Tombstoned(ctx, "movie", "blade-runner-2049|2017"); err != nil || got {
		t.Fatalf("an untouched work: tombstoned=%v err=%v, want false", got, err)
	}
}
