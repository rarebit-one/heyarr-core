package catalog_test

import (
	"context"
	"strings"
	"testing"
)

// The ensure-on-GET gate (#371, §19).
//
// IsBlobDesired is what stops a client GET from turning into fetch-on-request:
// only a blob the catalog still accounts for through a live asset may have its
// transfer started on demand. It answers out of the SAME predicate the
// canonical blob set uses, so these assert that the gate and that set agree —
// a blob a reconcile cycle would replicate is exactly a blob a GET may ensure.

func TestIsBlobDesiredMatchesTheCanonicalSet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	desired := "blake3:" + strings.Repeat("a", 64)
	absent := "blake3:" + strings.Repeat("b", 64)
	h.seedSatisfying(t, desired, 1080, "release")

	got, err := h.cat.IsBlobDesired(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("a blob a live asset references must be desired: the gate would 404 a blob reconciliation would replicate")
	}

	got, err = h.cat.IsBlobDesired(ctx, absent)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("a blob no asset references must NOT be desired: this is the gate that keeps a GET from fetching arbitrary content")
	}
}

// A blob whose bytes have gone (missing_since set) is out of the canonical set
// — canonicalBlobs excludes it so replication does not go and fetch bytes an
// asset has already reported lost. The gate must agree, or a GET would re-start
// a transfer for exactly the bytes the catalog stopped desiring.
func TestABlobWithMissingBytesIsNotDesired(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	hash := "blake3:" + strings.Repeat("c", 64)
	h.seedSatisfying(t, hash, 1080, "release")
	h.exec(t, `UPDATE assets SET missing_since = ? WHERE blob_hash = ?`, stamp, hash)

	got, err := h.cat.IsBlobDesired(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("a blob whose asset reports its bytes missing must not be desired: the gate must track the canonical set, missing_since and all")
	}
}
