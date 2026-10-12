package catalog_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/thumbnail"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// thumbBlobs used by thumbnails tests.
var (
	thumbSrcBlob  = "blake3:" + strings.Repeat("d", 64)
	thumbDstBlob  = "blake3:" + strings.Repeat("e", 64)
	thumbDstBlob2 = "blake3:" + strings.Repeat("f", 64)
)

// seedThumbSource seeds the minimum required for thumbnail recording: a blob
// in the blobs table that can be used as source_hash, and a second blob for
// the derived thumb.
func seedThumbSource(s *bookSeed) {
	s.t.Helper()
	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES
		(?, 100, 'image/jpeg', ?),
		(?, 200, 'image/jpeg', ?) ON CONFLICT DO NOTHING`,
		thumbSrcBlob, s.stamp, thumbDstBlob, s.stamp)
}

func TestThumbnailStatusReturnsEmptyStringWhenNoRow(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)
	seedThumbSource(s)

	status, hash, err := s.cat.ThumbnailStatus(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe)
	if err != nil {
		t.Fatalf("ThumbnailStatus: %v", err)
	}
	if status != "" || hash != "" {
		t.Errorf("want empty status and hash for absent row, got status=%q hash=%q", status, hash)
	}
}

func TestRecordThumbnailReadyEmitsEvent(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)
	seedThumbSource(s)

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := s.cat.RecordThumbnailReady(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe, thumbDstBlob, 1234, now); err != nil {
		t.Fatalf("RecordThumbnailReady: %v", err)
	}

	// Status is now ready.
	status, hash, err := s.cat.ThumbnailStatus(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe)
	if err != nil {
		t.Fatalf("ThumbnailStatus after ready: %v", err)
	}
	if status != catalog.ThumbnailStatusReady {
		t.Errorf("status = %q, want %q", status, catalog.ThumbnailStatusReady)
	}
	if hash != thumbDstBlob {
		t.Errorf("thumb_hash = %q, want %q", hash, thumbDstBlob)
	}

	// The thumbnail is a registered blob with this peer's present replica, so the
	// blob route, integrity and the collector see it (Codex on #744).
	var size int64
	var replicaState string
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT b.size, r.state FROM blobs b
		 JOIN replicas r ON r.blob_hash = b.hash
		 JOIN peers p ON p.id = r.peer_id AND p.is_self = 1
		 WHERE b.hash = ?`, thumbDstBlob).Scan(&size, &replicaState); err != nil {
		t.Fatalf("thumbnail blob/replica row: %v", err)
	}
	if size <= 0 || replicaState != "present" {
		t.Errorf("blob size %d replica %q, want a sized blob with a present replica", size, replicaState)
	}

	// An event was emitted.
	var count int
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT COUNT(*) FROM events WHERE type = ?`, events.TypeThumbnailReady).Scan(&count); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 1 {
		t.Errorf("event count = %d, want 1", count)
	}
}

func TestRecordThumbnailUndecodableEmitsEvent(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)
	seedThumbSource(s)

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := s.cat.RecordThumbnailUndecodable(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe, now); err != nil {
		t.Fatalf("RecordThumbnailUndecodable: %v", err)
	}

	status, hash, err := s.cat.ThumbnailStatus(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe)
	if err != nil {
		t.Fatalf("ThumbnailStatus after undecodable: %v", err)
	}
	if status != catalog.ThumbnailStatusUndecodable {
		t.Errorf("status = %q, want %q", status, catalog.ThumbnailStatusUndecodable)
	}
	if hash != "" {
		t.Errorf("thumb_hash = %q, want empty for undecodable", hash)
	}

	var count int
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT COUNT(*) FROM events WHERE type = ?`, events.TypeThumbnailUndecodable).Scan(&count); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 1 {
		t.Errorf("event count = %d, want 1", count)
	}
}

// TestRecipeBumpDeletesStaleRowsAndEmitsDeleted verifies that inserting a ready
// row for recipe N+1 removes the old recipe N row in the same transaction and
// emits TypeThumbnailDeleted with reason="recipe_superseded".
func TestRecipeBumpDeletesStaleRowsAndEmitsDeleted(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)

	// Insert the thumb blobs we need.
	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES
		(?, 100, 'image/jpeg', ?),
		(?, 200, 'image/jpeg', ?),
		(?, 300, 'image/jpeg', ?) ON CONFLICT DO NOTHING`,
		thumbSrcBlob, s.stamp, thumbDstBlob, s.stamp, thumbDstBlob2, s.stamp)

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	staleRecipe := thumbnail.Recipe - 1
	if staleRecipe < 0 {
		staleRecipe = thumbnail.Recipe + 1
	}

	// Seed a stale recipe row directly.
	s.exec(`INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at)
		VALUES (?, 256, ?, 'ready', ?, ?)`, thumbSrcBlob, staleRecipe, thumbDstBlob, s.stamp)

	// Record ready for the current recipe — this should delete the stale row.
	if err := s.cat.RecordThumbnailReady(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe, thumbDstBlob2, 1234, now); err != nil {
		t.Fatalf("RecordThumbnailReady: %v", err)
	}

	// Only the current-recipe row should survive.
	var count int
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT COUNT(*) FROM derived_thumbnails WHERE source_hash = ? AND bucket = 256`, thumbSrcBlob).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Errorf("derived_thumbnails row count = %d, want 1 (stale recipe must be gone)", count)
	}

	// A TypeThumbnailDeleted event with reason=recipe_superseded was emitted.
	var payload string
	err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT payload FROM events WHERE type = ? ORDER BY created_at LIMIT 1`,
		events.TypeThumbnailDeleted).Scan(&payload)
	if err == sql.ErrNoRows {
		t.Fatal("no TypeThumbnailDeleted event emitted")
	}
	if err != nil {
		t.Fatalf("reading event payload: %v", err)
	}
	if !strings.Contains(payload, "recipe_superseded") {
		t.Errorf("payload = %q; want reason=recipe_superseded", payload)
	}
}

// TestDeleteThumbnailsForSourceEmitsDeletedPerRow verifies that
// DeleteThumbnailsForSource removes all rows for a source and emits one
// TypeThumbnailDeleted event per row.
func TestDeleteThumbnailsForSourceEmitsDeletedPerRow(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)

	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES
		(?, 100, 'image/jpeg', ?),
		(?, 200, 'image/jpeg', ?) ON CONFLICT DO NOTHING`,
		thumbSrcBlob, s.stamp, thumbDstBlob, s.stamp)

	// Two rows: bucket 256 and 512.
	s.exec(`INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at) VALUES
		(?, 256, ?, 'ready',       ?, ?),
		(?, 512, ?, 'undecodable', NULL, ?)`,
		thumbSrcBlob, thumbnail.Recipe, thumbDstBlob, s.stamp,
		thumbSrcBlob, thumbnail.Recipe, s.stamp)

	tx, err := s.db.Writer().BeginTx(s.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.cat.DeleteThumbnailsForSource(s.ctx, tx, thumbSrcBlob)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("DeleteThumbnailsForSource: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if len(evs) != 2 {
		t.Errorf("events returned = %d, want 2 (one per row)", len(evs))
	}
	for _, ev := range evs {
		if ev.Type != events.TypeThumbnailDeleted {
			t.Errorf("event type = %q, want %q", ev.Type, events.TypeThumbnailDeleted)
		}
	}

	var remaining int
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT COUNT(*) FROM derived_thumbnails WHERE source_hash = ?`, thumbSrcBlob).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("remaining rows = %d, want 0", remaining)
	}
}

// TestGCRefCountDropsAfterThumbnailDeletion verifies ADR-0018 one-way GC:
// a ready derived_thumbnails row holds the thumb blob's reference count > 0,
// and after DeleteThumbnailsForSource the count drops.
func TestGCRefCountDropsAfterThumbnailDeletion(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)

	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES
		(?, 100, 'image/jpeg', ?),
		(?, 200, 'image/jpeg', ?) ON CONFLICT DO NOTHING`,
		thumbSrcBlob, s.stamp, thumbDstBlob, s.stamp)

	// Record a ready thumbnail.
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := s.cat.RecordThumbnailReady(s.ctx, thumbSrcBlob, 256, thumbnail.Recipe, thumbDstBlob, 1234, now); err != nil {
		t.Fatalf("RecordThumbnailReady: %v", err)
	}

	// The thumb blob must appear with References == 1 in the GC listing.
	blobs, err := s.cat.Blobs(s.ctx)
	if err != nil {
		t.Fatalf("Blobs: %v", err)
	}
	findRefs := func(hash string) int {
		for _, b := range blobs {
			if b.Hash.String() == hash {
				return b.References
			}
		}
		return -1 // not found
	}
	if got := findRefs(thumbDstBlob); got != 1 {
		t.Errorf("thumb blob References = %d, want 1 (held by ready row)", got)
	}

	// Delete all derivations for the source.
	tx, err := s.db.Writer().BeginTx(s.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.cat.DeleteThumbnailsForSource(s.ctx, tx, thumbSrcBlob); err != nil {
		_ = tx.Rollback()
		t.Fatalf("DeleteThumbnailsForSource: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// The thumb blob's reference count must now be 0 (GC-eligible).
	blobs, err = s.cat.Blobs(s.ctx)
	if err != nil {
		t.Fatalf("Blobs after delete: %v", err)
	}
	if got := findRefs(thumbDstBlob); got != 0 {
		t.Errorf("thumb blob References after deletion = %d, want 0 (should be GC-eligible)", got)
	}
}
