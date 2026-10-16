package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// ownedSpace is a space of the given kind created from the owner's device —
// the creation records the owner as its first member (ADR-0108).
func (h *grantHarness) ownedSpace(t *testing.T, kind spaces.Kind, owner string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := h.s.PutSpaceOwnedFrom(context.Background(), id, kind, owner, "ed25519:owner-dev"); err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *grantHarness) eventsOfType(t *testing.T, typ string) int {
	t.Helper()
	list, err := h.log.Since(context.Background(), 0, []string{typ}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// A space created from a device is its owner's from the first row: the owner
// is a member (RoleOwner) and the space is gated. A space a bearer created, or
// one from before ADR-0108, has no rows and is not.
func TestCreationRecordsTheOwnerAsAMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)

	owned := h.ownedSpace(t, spaces.KindShared, "owner")
	members, err := h.s.Members(ctx, owned)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].PrincipalID != "owner" || members[0].Role != store.RoleOwner ||
		members[0].AddedByDevice != "ed25519:owner-dev" || !members[0].Current() {
		t.Fatalf("members after creation = %+v, want the owner row", members)
	}
	ms, err := h.s.SpaceMembership(ctx, owned)
	if err != nil {
		t.Fatal(err)
	}
	if !ms.Gated() || !ms.Current["owner"] || ms.Kind != spaces.KindShared {
		t.Errorf("membership = %+v, want gated with the owner current", ms)
	}

	legacy := h.space(t, "")
	members, err = h.s.Members(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Errorf("a legacy space has members: %+v", members)
	}
	if ms, _ = h.s.SpaceMembership(ctx, legacy); ms.Gated() {
		t.Errorf("a legacy space is gated: %+v", ms)
	}
	if _, err := h.s.Members(ctx, uuid.Must(uuid.NewV7()).String()); !errors.Is(err, store.ErrUnknownSpace) {
		t.Errorf("members of an unknown space: %v", err)
	}
}

// The membership lifecycle (ADR-0108): an add is an event, a second add of the
// same user is idempotent and silent, a removal is a tombstone the list still
// shows and the gate reads as removed, and a re-add clears it.
func TestMembershipLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	id := h.ownedSpace(t, spaces.KindPersonal, "owner")

	m, err := h.s.AddMember(ctx, id, "other", "owner", "ed25519:owner-dev")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if m.Role != store.RoleMember || m.PrincipalID != "other" || m.AddedByPrincipal != "owner" || m.AddedByDevice != "ed25519:owner-dev" || !m.Current() {
		t.Fatalf("added = %+v", m)
	}
	if n := h.eventsOfType(t, events.TypeSpaceMemberAdded); n != 1 {
		t.Errorf("member_added events = %d, want 1", n)
	}
	// Idempotent: the row comes back and no second event is emitted.
	if again, err := h.s.AddMember(ctx, id, "other", "owner", "ed25519:owner-dev"); err != nil || !again.Current() {
		t.Fatalf("second add: %+v, %v", again, err)
	}
	if n := h.eventsOfType(t, events.TypeSpaceMemberAdded); n != 1 {
		t.Errorf("a repeated add emitted: %d events", n)
	}
	// Adding the owner is a no-op too: they are already in, as owner.
	if o, err := h.s.AddMember(ctx, id, "owner", "owner", "d"); err != nil || o.Role != store.RoleOwner {
		t.Fatalf("adding the owner: %+v, %v", o, err)
	}

	ms, err := h.s.SpaceMembership(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !ms.Current["owner"] || !ms.Current["other"] || len(ms.Removed) != 0 {
		t.Fatalf("membership = %+v", ms)
	}

	removed, err := h.s.RemoveMember(ctx, id, "other", "owner", "ed25519:owner-dev")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed.Current() || removed.RemovedAt.IsZero() {
		t.Fatalf("removed = %+v, want a tombstone", removed)
	}
	if n := h.eventsOfType(t, events.TypeSpaceMemberRemoved); n != 1 {
		t.Errorf("member_removed events = %d, want 1", n)
	}
	ms, _ = h.s.SpaceMembership(ctx, id)
	if ms.Current["other"] || !ms.Removed["other"] || !ms.Gated() {
		t.Fatalf("membership after removal = %+v", ms)
	}
	list, _ := h.s.Members(ctx, id)
	if len(list) != 2 || list[0].Role != store.RoleOwner || list[1].PrincipalID != "other" || list[1].Current() {
		t.Fatalf("list after removal = %+v, want the owner then the tombstone", list)
	}
	// Removing again is ErrNotMember, not a silent success.
	if _, err := h.s.RemoveMember(ctx, id, "other", "owner", "d"); !errors.Is(err, store.ErrNotMember) {
		t.Errorf("second removal: %v", err)
	}
	if _, err := h.s.RemoveMember(ctx, id, "never", "owner", "d"); !errors.Is(err, store.ErrNotMember) {
		t.Errorf("removing a stranger: %v", err)
	}
	// A re-add clears the tombstone and is an event again.
	back, err := h.s.AddMember(ctx, id, "other", "owner", "d")
	if err != nil || !back.Current() {
		t.Fatalf("re-add: %+v, %v", back, err)
	}
	if n := h.eventsOfType(t, events.TypeSpaceMemberAdded); n != 2 {
		t.Errorf("member_added events after re-add = %d, want 2", n)
	}
	ms, _ = h.s.SpaceMembership(ctx, id)
	if !ms.Current["other"] || ms.Removed["other"] {
		t.Fatalf("membership after re-add = %+v", ms)
	}
}

// Who may change membership (ADR-0108 via ADR-0104): the owner's device on a
// personal/shared/research space, anyone on a family space or a legacy space
// with no owner. The owner is never removable. Unknown spaces and empty
// members are refused distinctly.
func TestOnlyTheOwnerChangesMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	owned := h.ownedSpace(t, spaces.KindShared, "owner")
	family := h.ownedSpace(t, spaces.KindFamily, "owner")
	// A personal space with no recorded owner: a bearer token's, or one from
	// before ADR-0104.
	legacy := uuid.Must(uuid.NewV7()).String()
	if _, err := h.s.PutSpaceOwned(ctx, legacy, spaces.KindPersonal, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := h.s.AddMember(ctx, owned, "exec", "other", "d"); !errors.Is(err, store.ErrNotSpaceOwner) {
		t.Errorf("a non-owner added a member to a shared space: %v", err)
	}
	if _, err := h.s.AddMember(ctx, owned, "other", "owner", "d"); err != nil {
		t.Fatalf("the owner adding: %v", err)
	}
	if _, err := h.s.RemoveMember(ctx, owned, "other", "other", "d"); !errors.Is(err, store.ErrNotSpaceOwner) {
		t.Errorf("a member removed themselves from a shared space: %v", err)
	}
	if _, err := h.s.RemoveMember(ctx, owned, "owner", "owner", "d"); !errors.Is(err, store.ErrOwnerNotRemovable) {
		t.Errorf("the owner was removed: %v", err)
	}

	// A family space: any authorised device, and its rows never gate.
	if _, err := h.s.AddMember(ctx, family, "exec", "other", "d"); err != nil {
		t.Errorf("a family space refused a non-owner's add: %v", err)
	}
	if ms, _ := h.s.SpaceMembership(ctx, family); ms.Gated() || !ms.Current["exec"] {
		t.Errorf("family membership = %+v, want recorded but ungated", ms)
	}
	if _, err := h.s.RemoveMember(ctx, family, "owner", "other", "d"); !errors.Is(err, store.ErrOwnerNotRemovable) {
		t.Errorf("a family space's owner was removed: %v", err)
	}

	// A legacy space: anyone, no owner row appears, and the first add turns the
	// gate on for members only.
	if _, err := h.s.AddMember(ctx, legacy, "other", "exec", "d"); err != nil {
		t.Fatalf("a legacy space refused an add: %v", err)
	}
	list, _ := h.s.Members(ctx, legacy)
	if len(list) != 1 || list[0].Role != store.RoleMember {
		t.Errorf("legacy members = %+v, want one member and no owner row", list)
	}
	if ms, _ := h.s.SpaceMembership(ctx, legacy); !ms.Gated() {
		t.Errorf("a legacy space with a member is not gated: %+v", ms)
	}

	if _, err := h.s.AddMember(ctx, uuid.Must(uuid.NewV7()).String(), "other", "owner", "d"); !errors.Is(err, store.ErrUnknownSpace) {
		t.Errorf("adding to an unknown space: %v", err)
	}
	if _, err := h.s.AddMember(ctx, owned, "", "owner", "d"); !errors.Is(err, store.ErrEmptyMember) {
		t.Errorf("adding nobody: %v", err)
	}
}

// A space owned before membership existed has an owner but no rows. Its first
// membership change writes the owner's row, so the gate it turns on admits the
// owner's own devices.
func TestFirstChangeOnAPreMembershipSpaceRecordsTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	id := h.space(t, "owner") // PutSpaceOwned with no device: the pre-ADR-0108 shape
	if _, err := h.db.Writer().Exec(`DELETE FROM space_members WHERE space_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if ms, _ := h.s.SpaceMembership(ctx, id); ms.Gated() {
		t.Fatalf("precondition: a space with no rows is gated: %+v", ms)
	}
	if _, err := h.s.AddMember(ctx, id, "other", "owner", "d"); err != nil {
		t.Fatal(err)
	}
	ms, _ := h.s.SpaceMembership(ctx, id)
	if !ms.Current["owner"] || !ms.Current["other"] {
		t.Fatalf("membership = %+v, want the owner written alongside the member", ms)
	}
	list, _ := h.s.Members(ctx, id)
	if len(list) != 2 || list[0].Role != store.RoleOwner || list[0].PrincipalID != "owner" {
		t.Fatalf("list = %+v, want the owner row first", list)
	}
}
