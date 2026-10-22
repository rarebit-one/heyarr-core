package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Purging a media node's legacy vault data (#823): a preview changes nothing, a
// purge unpins everything, retires every taken placement and releases what
// nothing else wants, a second purge is a no-op, and library media is unpinned
// but never released.
func TestPurgingPlacementPinsReleasesLegacyVaultOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	vaultA, vaultB, media, taken, takenOnly := hashOf('a'), hashOf('b'), hashOf('e'), hashOf('f'), hashOf('9')
	h.seedBlobs(t, vaultA, vaultB, media, taken, takenOnly)
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
	// Blobs taken from two siblings, one also pinned here: a media node runs no
	// placement exchange, so nothing else would ever withdraw them.
	const otherSibling = "01990000-0000-7000-8000-0000000other"
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES (?, 'other-peer', 'site-c', 'full', 0, ?, ?)`, otherSibling, stamp, stamp)
	for _, tp := range [][2]string{{remotePeer, taken}, {remotePeer, takenOnly}, {otherSibling, takenOnly}} {
		h.exec(t, `INSERT INTO peer_placements (source_peer_id, blob_hash, size, received_at) VALUES (?, ?, 1024, ?)`,
			tp[0], tp[1], stamp)
	}

	preview, err := h.cat.PlacementPinPurgePreview(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.PinPurge{Pins: 5, Taken: 3, Blobs: 5, Bytes: 5 * 1024, Releasable: 4, StillWanted: 1}
	if preview != want {
		t.Fatalf("preview = %+v, want %+v", preview, want)
	}
	pins, err := h.cat.AllPlacementPins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 5 {
		t.Fatalf("the preview changed the pins: %d left, want 5", len(pins))
	}
	for _, blob := range []string{vaultA, vaultB, media, taken, takenOnly} {
		if b := h.releaseOf(t, blob); !b.ReleasedAt.IsZero() {
			t.Fatalf("the preview released %s", blob)
		}
	}

	got, err := h.cat.PurgePlacementPins(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("purge = %+v, want %+v", got, want)
	}
	if pins, err = h.cat.AllPlacementPins(ctx); err != nil || len(pins) != 0 {
		t.Fatalf("pins left after the purge: %d (%v)", len(pins), err)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM peer_placements`); n != 0 {
		t.Fatalf("taken placements left after the purge: %d", n)
	}
	// One exchange event per source for what was retired from it, never one
	// per blob.
	for source, removed := range map[string]int{remotePeer: 2, otherSibling: 1} {
		if n := h.countRows(t, `SELECT COUNT(*) FROM events WHERE type = ? AND subject_id = ?
			AND json_extract(payload, '$.removed') = ?`, events.TypeSyncPlacementsExchanged, source, removed); n != 1 {
			t.Fatalf("%d %s events for %s removing %d, want 1", n, events.TypeSyncPlacementsExchanged, source, removed)
		}
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM events WHERE type = ?`, events.TypeSyncPlacementsExchanged); n != 2 {
		t.Fatalf("%d %s events, want one per source (2)", n, events.TypeSyncPlacementsExchanged)
	}
	for blob, cause := range map[string]string{vaultA: "unpinned", vaultB: "unpinned", taken: "unpinned", takenOnly: "withdrawn"} {
		if b := h.releaseOf(t, blob); b.ReleasedAt.IsZero() || b.ReleaseCause != cause {
			t.Fatalf("legacy vault blob %s: release %+v, want cause %q", blob, b, cause)
		}
	}
	if b := h.releaseOf(t, media); !b.ReleasedAt.IsZero() {
		t.Fatal("the purge released library media")
	}

	again, err := h.cat.PurgePlacementPins(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again != (catalog.PinPurge{}) {
		t.Fatalf("a second purge = %+v, want a no-op", again)
	}
}

// A pre-#659 upload left bytes and a pin but no blobs row. The purge adopts
// those bytes before it releases them, so GC waits out the grace window instead
// of judging them as untracked by their age (#823). A pin whose bytes never
// arrived is removed with nothing to adopt.
func TestPurgingPlacementPinsAdoptsPinnedUntrackedBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	untracked, missing := hashOf('c'), hashOf('d')
	for _, blob := range []string{untracked, missing} {
		if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
			t.Fatal(err)
		}
	}
	held := func(_ context.Context, hash string) (int64, bool, error) {
		return 2048, hash == untracked, nil
	}

	preview, err := h.cat.PlacementPinPurgePreview(ctx, held)
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.PinPurge{Pins: 2, Blobs: 2, Bytes: 2048, Adopted: 1, Releasable: 2}
	if preview != want {
		t.Fatalf("preview = %+v, want %+v", preview, want)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM blobs`); n != 0 {
		t.Fatalf("the preview adopted %d blobs", n)
	}

	got, err := h.cat.PurgePlacementPins(ctx, held)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("purge = %+v, want %+v", got, want)
	}
	b := h.releaseOf(t, untracked)
	if b.Size != 2048 || b.ReleasedAt.IsZero() {
		t.Fatalf("the untracked bytes were not adopted and released: %+v", b)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM blobs WHERE hash = ?`, missing); n != 0 {
		t.Fatal("the purge adopted bytes this node does not hold")
	}
}

func (h *harness) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}
