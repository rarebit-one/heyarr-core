package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/events"
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

// releasedEvidence is what the collector records for a released blob: the
// release and the grace-window mark it observed.
func (h *harness) releasedEvidence(t *testing.T, blob string, at time.Time) integrity.Evidence {
	t.Helper()
	b := h.releaseOf(t, blob)
	return integrity.Evidence{
		BlobHash: b.Hash, Size: 1024, Basis: integrity.BasisReleased, RecordedAt: at,
		ReleasedAt: b.ReleasedAt, UnreferencedSince: b.UnreferencedSince,
	}
}

// markOnce is the sweep that starts a released blob's grace window.
func (h *harness) markOnce(t *testing.T, blob string, at time.Time) {
	t.Helper()
	parsed, err := hashing.Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.MarkUnreferenced(context.Background(), []hashing.Hash{parsed}, at); err != nil {
		t.Fatal(err)
	}
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
	// The sweep after the release starts its grace window, as a real GC does.
	if err := h.cat.MarkUnreferenced(ctx, []hashing.Hash{parsed}, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if err := h.cat.RecordDurability(ctx, h.releasedEvidence(t, blob, at)); err != nil {
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

// The reclaim revalidates a `released` basis in its own transaction: a pin that
// came back after the sweep read the release stops the delete, and the
// evidence written for the attempt is withdrawn (#779).
func TestReclaimRefusesAReleaseThatWentStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('9')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	parsed, _ := hashing.Parse(blob)
	at := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	h.markOnce(t, blob, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC))
	if err := h.cat.RecordDurability(ctx, h.releasedEvidence(t, blob, at)); err != nil {
		t.Fatal(err)
	}
	// The owner pins it again before the delete.
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}

	err := h.cat.Reclaim(ctx, parsed, 1024, true, at)
	if !errors.Is(err, integrity.ErrWantedAgain) {
		t.Fatalf("Reclaim = %v, want ErrWantedAgain", err)
	}
	known, err := h.cat.Known(ctx, []hashing.Hash{parsed})
	if err != nil {
		t.Fatal(err)
	}
	if !known[blob] {
		t.Fatal("the re-pinned blob's row was deleted")
	}
	var n int
	if err := h.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM durability_evidence WHERE blob_hash = ? AND basis = 'released'`, blob).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the refused attempt's released evidence was kept, and could vouch for a later delete")
	}
}

// Wanted again, then released again: the grace window starts from the new
// release, never from a mark left by an earlier unreferenced spell (#779).
func TestARenewedWantRestartsTheGraceWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('8')
	h.seedBlobs(t, blob)
	parsed, _ := hashing.Parse(blob)
	marked := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	mark := func() {
		t.Helper()
		if err := h.cat.MarkUnreferenced(ctx, []hashing.Hash{parsed}, marked); err != nil {
			t.Fatal(err)
		}
	}

	mark()
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if b := h.releaseOf(t, blob); !b.UnreferencedSince.IsZero() {
		t.Fatalf("a re-pinned blob kept its grace-window mark from %v", b.UnreferencedSince)
	}

	mark() // a sweep that has not yet seen the pin
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	b := h.releaseOf(t, blob)
	if b.ReleasedAt.IsZero() {
		t.Fatal("the unpin recorded no release")
	}
	if !b.UnreferencedSince.IsZero() {
		t.Fatalf("the release inherited a grace-window mark from %v", b.UnreferencedSince)
	}
}

// Overlapping sweeps: newer evidence on another basis must not mask this
// attempt's `released` basis. The reclaim checks its own attempt's evidence
// (by its timestamp) and the blob's references, not whichever row sorts last.
func TestReclaimIsNotFooledByNewerEvidenceOnAnotherBasis(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		wantBack func(t *testing.T, h *harness, blob string, parsed hashing.Hash, self string)
	}{
		{"pinned again", func(t *testing.T, h *harness, blob string, _ hashing.Hash, self string) {
			if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
				t.Fatal(err)
			}
		}},
		{"release cleared, nothing referencing it now", func(t *testing.T, h *harness, _ string, parsed hashing.Hash, _ string) {
			if err := h.cat.ClearUnreferenced(ctx, []hashing.Hash{parsed}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			self := h.seedRemotePeer(t)
			blob := hashOf('7')
			h.seedBlobs(t, blob)
			parsed, _ := hashing.Parse(blob)
			if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
				t.Fatal(err)
			}
			if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
				t.Fatal(err)
			}
			h.markOnce(t, blob, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC))
			if err := h.cat.RecordDurability(ctx, h.releasedEvidence(t, blob, at)); err != nil {
				t.Fatal(err)
			}
			tc.wantBack(t, h, blob, parsed, self)
			// A concurrent, non-honouring sweep records newer evidence.
			if err := h.cat.RecordDurability(ctx, integrity.Evidence{
				BlobHash: parsed, Size: 1024, Basis: integrity.BasisVerifiedRemote, RecordedAt: at.Add(time.Second),
			}); err != nil {
				t.Fatal(err)
			}

			if err := h.cat.Reclaim(ctx, parsed, 1024, true, at); !errors.Is(err, integrity.ErrWantedAgain) {
				t.Fatalf("Reclaim = %v, want ErrWantedAgain", err)
			}
			known, err := h.cat.Known(ctx, []hashing.Hash{parsed})
			if err != nil {
				t.Fatal(err)
			}
			if !known[blob] {
				t.Fatal("the blob's row was deleted on a release that no longer stood")
			}
		})
	}
}

// Both release transitions are events, in the transactions that make them.
func TestReleaseTransitionsAreEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('6')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if n := h.eventsOfType(t, events.TypeBlobReleaseCleared); n != 0 {
		t.Fatalf("pinning an unreleased blob emitted %d blob.release_cleared", n)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if n := h.eventsOfType(t, events.TypeBlobReleased); n != 1 {
		t.Fatalf("the last unpin emitted %d blob.released, want 1", n)
	}
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if n := h.eventsOfType(t, events.TypeBlobReleaseCleared); n != 1 {
		t.Fatalf("re-pinning a released blob emitted %d blob.release_cleared, want 1", n)
	}
}

// A blob re-pinned and released again while a sweep is between reading it and
// reclaiming it is a new release generation: the old sweep's attempt must not
// honour it, because its grace window has not run (#779).
func TestReclaimRefusesANewerReleaseGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('5')
	h.seedBlobs(t, blob)
	parsed, _ := hashing.Parse(blob)
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	// The first sweep after the release starts its window.
	if err := h.cat.MarkUnreferenced(ctx, []hashing.Hash{parsed}, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	decided := h.releasedEvidence(t, blob, at)
	if err := h.cat.RecordDurability(ctx, decided); err != nil {
		t.Fatal(err)
	}

	// Mid-sweep: pinned and unpinned again.
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.Reclaim(ctx, parsed, 1024, true, at); !errors.Is(err, integrity.ErrWantedAgain) {
		t.Fatalf("Reclaim = %v, want ErrWantedAgain for a release whose window has not run", err)
	}

	// Another sweep has since marked the new generation: still not the window
	// this attempt decided on, so still refused.
	if err := h.cat.RecordDurability(ctx, integrity.Evidence{
		BlobHash: parsed, Size: 1024, Basis: integrity.BasisReleased, RecordedAt: at,
		ReleasedAt: decided.ReleasedAt, UnreferencedSince: decided.UnreferencedSince,
	}); err != nil {
		t.Fatal(err)
	}
	h.markOnce(t, blob, at.Add(-time.Hour))
	if err := h.cat.Reclaim(ctx, parsed, 1024, true, at); !errors.Is(err, integrity.ErrWantedAgain) {
		t.Fatalf("Reclaim = %v, want ErrWantedAgain once another sweep marked the new generation", err)
	}
}

// GuardUnlink unlinks only when nothing references the blob, checked under the
// write lock, so a pin written after the row delete keeps the bytes (#779).
func TestGuardUnlinkRefusesABlobReferencedAfterItsRowWent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('4')
	parsed, _ := hashing.Parse(blob)

	called := false
	unlink := func() error { called = true; return nil }

	// No row and nothing referencing it: the unlink runs.
	if err := h.cat.GuardUnlink(ctx, parsed, unlink); err != nil || !called {
		t.Fatalf("unreferenced: err=%v unlinked=%v, want the unlink", err, called)
	}

	// A pin arrived (no foreign key needs the row): the unlink must not run.
	called = false
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.GuardUnlink(ctx, parsed, unlink); !errors.Is(err, integrity.ErrWantedAgain) || called {
		t.Fatalf("pinned: err=%v unlinked=%v, want ErrWantedAgain and no unlink", err, called)
	}
}
