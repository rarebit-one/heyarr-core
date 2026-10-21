package catalog_test

import (
	"context"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// Release intent (#779, ADR-0114): the last pin or taken placement going is the
// owner letting a vault blob go, and is recorded in the same transaction. A new
// pin or placement clears it, and library media is never released.

func (h *harness) releaseOf(t *testing.T, hash string) integrity.Blob {
	t.Helper()
	parsed, err := hashing.Parse(hash)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.cat.Blob(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUnpinningTheLastPinRecordsRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('a')
	h.seedBlobs(t, blob)
	for _, peer := range []string{self, remotePeer} {
		if err := h.cat.PinPlacement(ctx, blob, peer); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.cat.UnpinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, blob); !b.ReleasedAt.IsZero() {
		t.Fatal("released while another pin still wants the blob")
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	b := h.releaseOf(t, blob)
	if b.ReleasedAt.IsZero() || b.ReleaseCause != "unpinned" {
		t.Fatalf("after the last unpin: released_at=%v cause=%q, want a release caused by unpinned",
			b.ReleasedAt, b.ReleaseCause)
	}
	// Unpinning again (nothing there) is not an error and changes nothing.
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}

	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, blob); !b.ReleasedAt.IsZero() {
		t.Fatal("a re-pinned blob kept its release")
	}
}

func TestAWithdrawnPlacementRecordsReleaseOnlyWhenNothingElseWantsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	taken, alsoPinned := hashOf('b'), hashOf('c')
	entries := []placementsync.Entry{{BlobHash: taken, Size: 1024, Held: true}, {BlobHash: alsoPinned, Size: 1024, Held: true}}
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all, entries); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.PinPlacement(ctx, alsoPinned, self); err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all, nil); err != nil {
		t.Fatal(err)
	}
	b := h.releaseOf(t, taken)
	if b.ReleasedAt.IsZero() || b.ReleaseCause != "withdrawn" {
		t.Fatalf("withdrawn placement: released_at=%v cause=%q, want a release caused by withdrawn",
			b.ReleasedAt, b.ReleaseCause)
	}
	if b := h.releaseOf(t, alsoPinned); !b.ReleasedAt.IsZero() {
		t.Fatal("released a blob a local pin still wants")
	}

	// Offered again: wanted again, so the release goes.
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all, entries[:1]); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, taken); !b.ReleasedAt.IsZero() {
		t.Fatal("a re-offered placement kept its release")
	}
}

// Unlisting a sibling is topology, not an owner letting go: nothing is released.
func TestDroppingAnUnlistedSiblingRecordsNoRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	taken := hashOf('d')
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all,
		[]placementsync.Entry{{BlobHash: taken, Size: 1024, Held: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.DropPlacementsFromOthers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, taken); !b.ReleasedAt.IsZero() {
		t.Fatal("unlisting a sibling released what was taken from it")
	}
}

// Library media is never released: a blob a scanned library file reaches
// records no release when a stray pin on it goes, and a release row written by
// hand is not read for it either.
func TestLibraryMediaIsNeverReleased(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	media := hashOf('e')
	h.seedBlobs(t, media)
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-1', 'Films', 'movie', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root-1', 'lib-1', '/srv/films', 'link', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO scanned_files (root_id, path, size, mtime_ns, blob_hash, scanned_at)
		VALUES ('root-1', 'film.mkv', 1024, 1, ?, ?)`, media, stamp)
	if err := h.cat.PinPlacement(ctx, media, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, media, self); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, media); !b.ReleasedAt.IsZero() {
		t.Fatal("library media was released by an unpin")
	}
	h.exec(t, `INSERT INTO blob_releases (blob_hash, cause, released_at) VALUES (?, 'unpinned', ?)`, media, stamp)
	if b := h.releaseOf(t, media); !b.ReleasedAt.IsZero() {
		t.Fatal("a release row was read for a blob a scanned library file reaches")
	}
}

// A released blob's reclaim is recorded with the `released` basis, which the
// evidence table accepts, and the release goes with the blob.
func TestReclaimingAReleasedBlobRecordsTheBasisAndForgetsTheRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('f')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	parsed, _ := hashing.Parse(blob)
	at := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if err := h.cat.RecordDurability(ctx, integrity.Evidence{
		BlobHash: parsed, Size: 1024, Basis: integrity.BasisReleased, RecordedAt: at,
	}); err != nil {
		t.Fatalf("recording released evidence: %v", err)
	}
	if err := h.cat.Reclaim(ctx, parsed, 1024, true, at); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM blob_releases WHERE blob_hash = ?`, blob).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the release outlived the blob it described")
	}
}
