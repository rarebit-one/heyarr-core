package catalog_test

import (
	"testing"
)

// Follow-management grants against a real database (ADR-0061, M12). The domain
// question — "does a granted device carry write" — is answered in the http auth
// layer; what only a real database can be wrong about is here: that a grant round
// trips, that re-granting is idempotent rather than a unique-violation, that a
// revoke reports whether anything existed, and that both transitions emit.

func TestManagementGrantRoundTripsAndAuthorizes(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	const dev = "ed25519:phone"

	ok, err := h.cat.ManagementAuthorized(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an ungranted device must not be authorised")
	}

	before := h.eventCount(t)
	grant, err := h.cat.GrantManagement(ctx, dev, "the operator's phone")
	if err != nil {
		t.Fatal(err)
	}
	if grant.DeviceKey != dev || grant.Reason != "the operator's phone" {
		t.Fatalf("grant = %+v, want the device and reason back", grant)
	}
	if grant.GrantedAt.IsZero() {
		t.Error("a grant must be stamped")
	}
	if h.eventCount(t) != before+1 {
		t.Error("granting management is a transition and must emit (invariant 7)")
	}

	ok, err = h.cat.ManagementAuthorized(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a granted device must be authorised")
	}

	// A blank key is never authorised — it cannot have a row.
	if ok, err := h.cat.ManagementAuthorized(ctx, ""); err != nil || ok {
		t.Fatalf("a blank device key must never be authorised (ok=%v err=%v)", ok, err)
	}
}

func TestManagementGrantIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	const dev = "ed25519:phone"

	if _, err := h.cat.GrantManagement(ctx, dev, "first"); err != nil {
		t.Fatal(err)
	}
	// Re-granting updates the note rather than failing on the primary key.
	g, err := h.cat.GrantManagement(ctx, dev, "second")
	if err != nil {
		t.Fatalf("re-granting must be idempotent, got %v", err)
	}
	if g.Reason != "second" {
		t.Errorf("reason = %q, want the updated note", g.Reason)
	}

	grants, err := h.cat.ListManagementGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("re-granting the same device must not add a second row, got %d", len(grants))
	}

	if _, err := h.cat.GrantManagement(ctx, "", "blank"); err == nil {
		t.Error("a blank device key must be refused")
	}
}

func TestRevokeManagementReportsExistence(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	const dev = "ed25519:phone"

	// Revoking a device that was never granted is not an error, and reports false.
	existed, err := h.cat.RevokeManagement(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Error("revoking an ungranted device must report it did not exist")
	}

	if _, err := h.cat.GrantManagement(ctx, dev, ""); err != nil {
		t.Fatal(err)
	}
	before := h.eventCount(t)
	existed, err = h.cat.RevokeManagement(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("revoking a granted device must report it existed")
	}
	if h.eventCount(t) != before+1 {
		t.Error("revoking a grant is a transition and must emit (invariant 7)")
	}
	if ok, _ := h.cat.ManagementAuthorized(ctx, dev); ok {
		t.Error("a revoked device must no longer be authorised")
	}
}
