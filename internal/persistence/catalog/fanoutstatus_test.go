package catalog_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// TestIsSiblingKey: only a member other than this node counts, found by key.
// A stranger, this node itself and a malformed key do not.
func TestIsSiblingKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	sibling := h.seedSibling(t)
	strangerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	selfPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))

	for key, want := range map[string]bool{
		sibling:                               true,
		identity.FormatPublicKey(strangerPub): false,
		identity.FormatPublicKey(selfPub):     false,
		"not-a-key":                           false,
	} {
		got, err := h.cat.IsSiblingKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("IsSiblingKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// A want fan-out did not create has no fan-out status.
func TestFanOutStatusForALocalWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, found, err := h.cat.FanOutStatusFor(context.Background(), "no-such-want"); err != nil || found {
		t.Fatalf("FanOutStatusFor = found %v, err %v; want not found", found, err)
	}
}
