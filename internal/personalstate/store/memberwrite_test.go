package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

const (
	ownerDev = "x25519:0a01000000000000000000000000000000000000000000000000000000000000"
	otherDev = "x25519:0b01000000000000000000000000000000000000000000000000000000000000"
)

// ADR-0128: a device writes to a space as a member only while it holds a copy
// of the space's current key AND the membership gate admits its user. Each row
// is one space and one (principal, device) asking.
func TestMemberHoldsCurrentKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)

	wrap := func(t *testing.T, spaceID, recipient string, epoch int) {
		t.Helper()
		if _, err := h.s.PutWrappedKey(ctx, spaceID, recipient, []byte("k-"+recipient[7:11]), epoch); err != nil {
			t.Fatal(err)
		}
	}
	legacy := func(t *testing.T, kind spaces.Kind) string {
		t.Helper()
		id := uuid.Must(uuid.NewV7()).String()
		if _, err := h.s.PutSpace(ctx, id, kind); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// The owner's personal space, wrapped for the owner's device.
	personal := h.ownedSpace(t, spaces.KindPersonal, "owner")
	wrap(t, personal, ownerDev, 0)

	// A shared space, with other as a current member holding a copy.
	shared := h.ownedSpace(t, spaces.KindShared, "owner")
	wrap(t, shared, ownerDev, 0)
	if _, err := h.s.AddMember(ctx, shared, "other", "owner", "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}
	wrap(t, shared, otherDev, 0)

	// A shared space other WAS a member of: the copy is still there (no
	// rotation has run), but the tombstone refuses the write.
	removed := h.ownedSpace(t, spaces.KindShared, "owner")
	if _, err := h.s.AddMember(ctx, removed, "other", "owner", "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}
	wrap(t, removed, otherDev, 0)
	if _, err := h.s.RemoveMember(ctx, removed, "other", "owner", "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}

	// An owned space with no membership rows yet holding other's copy: write is
	// narrower than the wrap gate, so only the owner writes.
	// (A space owned before membership existed: its owner row is not there.)
	ownedNoRows := h.ownedSpace(t, spaces.KindShared, "owner")
	if _, err := h.db.Writer().Exec(`DELETE FROM space_members WHERE space_id = ?`, ownedNoRows); err != nil {
		t.Fatal(err)
	}
	wrap(t, ownedNoRows, otherDev, 0)
	wrap(t, ownedNoRows, ownerDev, 0)

	// A family space: every household user, gated by the key alone.
	family := h.ownedSpace(t, spaces.KindFamily, "owner")
	wrap(t, family, otherDev, 0)

	// A legacy household space (no owner, no rows): the key alone.
	household := legacy(t, spaces.KindPersonal)
	wrap(t, household, otherDev, 0)

	// A space rotated past the copy other holds: the current key is not its.
	rotated := legacy(t, spaces.KindPersonal)
	wrap(t, rotated, otherDev, 0)
	wrap(t, rotated, ownerDev, 0)
	if _, err := h.s.RotateKey(ctx, rotated, 0, []byte("prev"),
		[]store.RecipientWrap{{Recipient: ownerDev, Wrapped: []byte("k1")}}, []string{otherDev}, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, space, principal, recipient string
		want                              bool
	}{
		{"owner on their personal space", personal, "owner", ownerDev, true},
		{"a stranger on someone's personal space", personal, "other", otherDev, false},
		{"a member of a shared space", shared, "other", otherDev, true},
		{"a member's device without a copy", shared, "other", "x25519:0c", false},
		{"a removed member still holding a copy", removed, "other", otherDev, false},
		{"a non-owner on an owned space with no rows", ownedNoRows, "other", otherDev, false},
		{"the owner of an owned space with no rows", ownedNoRows, "owner", ownerDev, true},
		{"any household user on a family space", family, "other", otherDev, true},
		{"a key holder on a legacy household space", household, "other", otherDev, true},
		{"a device a rotation left out", rotated, "other", otherDev, false},
		{"the device the rotation re-wrapped", rotated, "owner", ownerDev, true},
		{"an unknown space", uuid.Must(uuid.NewV7()).String(), "owner", ownerDev, false},
		{"no device key", personal, "owner", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.s.MemberHoldsCurrentKey(ctx, tc.space, tc.principal, tc.recipient)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("MemberHoldsCurrentKey = %v, want %v", got, tc.want)
			}
		})
	}
}

// The vault routes ask "some space": true while one space admits the device,
// false once none does.
func TestMemberHoldsAnyCurrentKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)

	holds := func(principal, recipient string) bool {
		t.Helper()
		ok, err := h.s.MemberHoldsAnyCurrentKey(ctx, principal, recipient)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if holds("other", otherDev) {
		t.Fatal("a device holding no copy anywhere may write some space")
	}
	// A copy on a space other was removed from does not count.
	removed := h.ownedSpace(t, spaces.KindShared, "owner")
	if _, err := h.s.AddMember(ctx, removed, "other", "owner", "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.PutWrappedKey(ctx, removed, otherDev, []byte("k"), 0); err != nil {
		t.Fatal(err)
	}
	if !holds("other", otherDev) {
		t.Fatal("a current member holding a copy may not write any space")
	}
	if _, err := h.s.RemoveMember(ctx, removed, "other", "owner", "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}
	if holds("other", otherDev) {
		t.Fatal("a removed member's stale copy still counts")
	}
	// A family space admits it again.
	family := h.ownedSpace(t, spaces.KindFamily, "owner")
	if _, err := h.s.PutWrappedKey(ctx, family, otherDev, []byte("k"), 0); err != nil {
		t.Fatal(err)
	}
	if !holds("other", otherDev) {
		t.Fatal("a family space copy does not count")
	}
}
