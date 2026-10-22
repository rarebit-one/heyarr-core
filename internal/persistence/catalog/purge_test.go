package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Purging a media node's legacy vault pins (#823): a preview changes nothing, a
// purge unpins everything and releases what nothing else wants, a second purge
// is a no-op, and library media and taken placements are unpinned but never
// released.
func TestPurgingPlacementPinsReleasesLegacyVaultOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	vaultA, vaultB, media, taken := hashOf('a'), hashOf('b'), hashOf('e'), hashOf('f')
	h.seedBlobs(t, vaultA, vaultB, media, taken)
	// Legacy vault: vaultA pinned on two peers, vaultB on one.
	for _, pin := range [][2]string{{vaultA, self}, {vaultA, remotePeer}, {vaultB, self}, {media, self}, {taken, self}} {
		if err := h.cat.PinPlacement(ctx, pin[0], pin[1]); err != nil {
			t.Fatal(err)
		}
	}
	// Library media that also carries a stray pin.
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-1', 'Films', 'movie', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root-1', 'lib-1', '/srv/films', 'link', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO scanned_files (root_id, path, size, mtime_ns, blob_hash, scanned_at)
		VALUES ('root-1', 'film.mkv', 1024, 1, ?, ?)`, media, stamp)
	// A blob also taken from a sibling.
	h.exec(t, `INSERT INTO peer_placements (source_peer_id, blob_hash, size, received_at) VALUES (?, ?, 1024, ?)`,
		remotePeer, taken, stamp)

	preview, err := h.cat.PlacementPinPurgePreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Pins != 5 || preview.Blobs != 4 || preview.Releasable != 2 || preview.StillWanted != 2 {
		t.Fatalf("preview = %+v, want 5 pins over 4 blobs, 2 releasable and 2 still wanted", preview)
	}
	if preview.Bytes != 4*1024 {
		t.Fatalf("preview bytes = %d, want %d", preview.Bytes, 4*1024)
	}
	pins, err := h.cat.AllPlacementPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 5 {
		t.Fatalf("the preview changed the pins: %d left, want 5", len(pins))
	}
	for _, blob := range []string{vaultA, vaultB, media, taken} {
		if b := h.releaseOf(t, blob); !b.ReleasedAt.IsZero() {
			t.Fatalf("the preview released %s", blob)
		}
	}

	got, err := h.cat.PurgePlacementPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pins != 5 || got.Blobs != 4 || got.Releasable != 2 || got.StillWanted != 2 {
		t.Fatalf("purge = %+v, want 5 pins over 4 blobs, 2 released and 2 still wanted", got)
	}
	if pins, err = h.cat.AllPlacementPins(ctx); err != nil || len(pins) != 0 {
		t.Fatalf("pins left after the purge: %d (%v)", len(pins), err)
	}
	for _, blob := range []string{vaultA, vaultB} {
		if b := h.releaseOf(t, blob); b.ReleasedAt.IsZero() || b.ReleaseCause != "unpinned" {
			t.Fatalf("legacy vault blob %s not released: %+v", blob, b)
		}
	}
	if b := h.releaseOf(t, media); !b.ReleasedAt.IsZero() {
		t.Fatal("the purge released library media")
	}
	if b := h.releaseOf(t, taken); !b.ReleasedAt.IsZero() {
		t.Fatal("the purge released a blob still taken from a sibling")
	}

	again, err := h.cat.PurgePlacementPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != (catalog.PinPurge{}) {
		t.Fatalf("a second purge = %+v, want a no-op", again)
	}
}
