package client_test

import (
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// TestRotateReKeysForRemainingOnly is the forward-looking core of device
// revocation (§41, ADR-0022, ADR-0049): rotating a space for the REMAINING
// recipients re-keys it so the revoked device can read no NEW content, while the
// old key it already held still opens the OLD content — revocation is
// forward-looking, not retroactive.
func TestRotateReKeysForRemainingOnly(t *testing.T) {
	t.Parallel()
	_, ra, ua := dev(t)
	_, rb, ub := dev(t)

	m := client.New()
	sp, wrapped, err := m.Create(spaces.KindFamily, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}
	oldCt, err := m.Encrypt(sp.ID, []byte("old content"))
	if err != nil {
		t.Fatal(err)
	}

	// Revoke B: rotate for A only.
	newWrapped, err := m.Rotate(sp.ID, []client.Recipient{ra})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if len(newWrapped) != 1 || newWrapped[0].Recipient != ra.ID {
		t.Fatalf("rotation re-keyed for %v, want only A", newWrapped)
	}
	newCt, err := m.Encrypt(sp.ID, []byte("new content"))
	if err != nil {
		t.Fatal(err)
	}

	// A, opening the NEW wrapped copy on a fresh device view, reads the new content.
	mA := client.New()
	if err := mA.Open(sp.ID, newWrapped[0].Wrapped, ua); err != nil {
		t.Fatalf("A.Open(new): %v", err)
	}
	if got, _ := mA.Decrypt(sp.ID, newCt); string(got) != "new content" {
		t.Fatalf("A could not read post-rotation content: %q", got)
	}

	// B holds only its ORIGINAL wrapped copy (the old key). It can still read the
	// OLD content (forward-looking revocation) but NOT the new.
	mB := client.New()
	if err := mB.Open(sp.ID, wrappedForRecipient(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open(old): %v", err)
	}
	if got, err := mB.Decrypt(sp.ID, oldCt); err != nil || string(got) != "old content" {
		t.Fatalf("B should still read pre-rotation content: %q %v", got, err)
	}
	if _, err := mB.Decrypt(sp.ID, newCt); err == nil {
		t.Fatal("the revoked device read post-rotation content: forward secrecy broken")
	}
}

// TestRotateRequiresOpenSpaceAndRecipients: only a device that can read a space
// may re-key it, and it must re-key for at least one recipient.
func TestRotateRequiresOpenSpaceAndRecipients(t *testing.T) {
	t.Parallel()
	_, ra, _ := dev(t)
	m := client.New()
	if _, err := m.Rotate("never-opened", []client.Recipient{ra}); !errors.Is(err, client.ErrSpaceNotOpen) {
		t.Fatalf("Rotate on unopened = %v, want ErrSpaceNotOpen", err)
	}
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rotate(sp.ID, nil); err == nil {
		t.Fatal("Rotate accepted zero recipients")
	}
}

func wrappedForRecipient(t *testing.T, ws []client.WrappedFor, id string) []byte {
	t.Helper()
	for _, w := range ws {
		if w.Recipient == id {
			return w.Wrapped
		}
	}
	t.Fatalf("no wrapped key for %s", id)
	return nil
}
