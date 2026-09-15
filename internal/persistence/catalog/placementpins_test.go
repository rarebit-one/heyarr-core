package catalog_test

import (
	"context"
	"testing"
)

// refCount finds a blob in a Blobs() listing and returns its reference count.
func refCount(t *testing.T, h *harness, hash string) int {
	t.Helper()
	blobs, err := h.cat.Blobs(context.Background())
	if err != nil {
		t.Fatalf("Blobs: %v", err)
	}
	for _, b := range blobs {
		if b.Hash.String() == hash {
			return b.References
		}
	}
	t.Fatalf("blob %s not listed", hash)
	return -1
}

// TestPlacementPinCountsAsAReference is the retention that keeps a vault blob
// alive (ADR-0096): a vault blob has no `assets` row, so without a pin GC sees it
// as unreferenced and reclaims it. A placement pin must count as a reference so
// GC keeps it — and drop back to reclaimable when the pin is removed.
func TestPlacementPinCountsAsAReference(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := hashOf('a')
	h.seedBlobs(t, blob) // a blob with NO assets row — a vault blob

	// Unreferenced before any pin: GC would reclaim it.
	if got := refCount(t, h, blob); got != 0 {
		t.Fatalf("an unpinned vault blob should have 0 references, got %d", got)
	}

	// A pin makes it referenced — GC keeps it.
	if err := h.cat.PinPlacement(ctx, blob, "peer-self"); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}
	if got := refCount(t, h, blob); got != 1 {
		t.Fatalf("a pinned vault blob should have 1 reference, got %d", got)
	}

	// A pin for a second peer counts too; re-pinning is idempotent per (blob, peer).
	if err := h.cat.PinPlacement(ctx, blob, "peer-b"); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.PinPlacement(ctx, blob, "peer-b"); err != nil {
		t.Fatal(err)
	}
	if got := refCount(t, h, blob); got != 2 {
		t.Fatalf("two distinct pins should be 2 references (idempotent per peer), got %d", got)
	}

	// Removing the pins returns it to reclaimable.
	if err := h.cat.UnpinPlacement(ctx, blob, "peer-self"); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UnpinPlacement(ctx, blob, "peer-b"); err != nil {
		t.Fatal(err)
	}
	if got := refCount(t, h, blob); got != 0 {
		t.Fatalf("after unpinning, the blob should be reclaimable again (0 refs), got %d", got)
	}
}

// TestPinPlacementRejectsEmptyArgs: a pin needs both a blob and a peer.
func TestPinPlacementRejectsEmptyArgs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.cat.PinPlacement(ctx, "", "peer"); err == nil {
		t.Fatal("an empty blob hash must be refused")
	}
	if err := h.cat.PinPlacement(ctx, hashOf('a'), ""); err == nil {
		t.Fatal("an empty peer id must be refused")
	}
}
