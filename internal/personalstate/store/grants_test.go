package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

type grantHarness struct {
	db    *sqlite.DB
	s     *store.Store
	log   *events.Log
	clock *fixedClock
}

// newGrantHarness is a store over a database holding an owner (user), another
// user and two executor principals — grants reference real principals.
func newGrantHarness(t *testing.T) *grantHarness {
	t.Helper()
	db := testdb.Migrated(t)
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, kind string }{{"owner", "user"}, {"other", "user"}, {"exec", "executor"}, {"exec2", "executor"}} {
		if _, err := db.Writer().Exec(`INSERT INTO principals (id, kind, name, created_at) VALUES (?, ?, ?, ?)`,
			p.id, p.kind, p.id, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	return &grantHarness{db: db, s: s, log: log, clock: clock}
}

func (h *grantHarness) space(t *testing.T, owner string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := h.s.PutSpaceOwned(context.Background(), id, spaces.KindFamily, owner); err != nil {
		t.Fatal(err)
	}
	return id
}

// The grant lifecycle (ADR-0104): a grant is active until revoked, a re-grant
// renews it, and each transition is an event naming the device — never a key.
func TestAGrantIsActiveUntilRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.space(t, "owner")

	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); ok {
		t.Fatal("a grant exists before one was made")
	}
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil); err != nil {
		t.Fatal(err)
	}
	g, ok, err := h.s.ActiveGrant(ctx, sp, "exec")
	if err != nil || !ok || g.Writes() {
		t.Fatalf("after a read grant: %+v ok=%v err=%v", g, ok, err)
	}
	if has, _ := h.s.HasAnyWriteGrant(ctx, "exec"); has {
		t.Error("a read grant counts as a write grant")
	}
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsReadWrite, "owner", "ed25519:dev", nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := h.s.HasAnyWriteGrant(ctx, "exec"); !has {
		t.Error("a read,write grant is not a write grant")
	}
	set, err := h.s.GrantedSpaceIDs(ctx, "exec")
	if err != nil || !set[sp] || len(set) != 1 {
		t.Errorf("granted spaces = %v, %v", set, err)
	}

	if _, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); ok {
		t.Error("a revoked grant is still active")
	}
	if _, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev"); !errors.Is(err, store.ErrNoGrant) {
		t.Errorf("a second revoke: %v, want ErrNoGrant", err)
	}
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); !ok {
		t.Error("a re-grant did not clear the revocation")
	}

	granted := eventsOfType(t, h.log, events.TypeSpaceAccessGranted)
	revoked := eventsOfType(t, h.log, events.TypeSpaceAccessRevoked)
	if len(granted) != 3 || len(revoked) != 1 {
		t.Fatalf("events: %d granted, %d revoked; want 3 and 1", len(granted), len(revoked))
	}
	if granted[0].SubjectID != sp {
		t.Errorf("grant event subject = %q, want the space", granted[0].SubjectID)
	}
}

// Only the owner grants on an owned space; a legacy (ownerless) space admits any
// authorised device's user, which is today's household behaviour.
func TestOnlyTheOwnerGrants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	owned := h.space(t, "owner")
	legacy := h.space(t, "")

	if _, err := h.s.GrantAccess(ctx, owned, "exec", store.CapsRead, "other", "ed25519:dev2", nil); !errors.Is(err, store.ErrNotSpaceOwner) {
		t.Errorf("a non-owner granted on an owned space: %v", err)
	}
	if _, err := h.s.GrantAccess(ctx, legacy, "exec", store.CapsRead, "other", "ed25519:dev2", nil); err != nil {
		t.Errorf("a legacy space refused a grant: %v", err)
	}
	if _, err := h.s.GrantAccess(ctx, uuid.Must(uuid.NewV7()).String(), "exec", store.CapsRead, "owner", "d", nil); !errors.Is(err, store.ErrUnknownSpace) {
		t.Errorf("a grant on an unknown space: %v", err)
	}
	if _, err := h.s.GrantAccess(ctx, owned, "exec", "admin", "owner", "d", nil); !errors.Is(err, store.ErrInvalidCaps) {
		t.Errorf("an admin grant: %v", err)
	}
	// A re-push of the space never changes its owner.
	if _, err := h.s.PutSpaceOwned(ctx, owned, spaces.KindFamily, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.GrantAccess(ctx, owned, "exec", store.CapsRead, "other", "d", nil); !errors.Is(err, store.ErrNotSpaceOwner) {
		t.Errorf("a re-push transferred ownership: %v", err)
	}
}

// An expired grant is inactive on every read of it (plan risk R2).
func TestAnExpiredGrantIsInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.space(t, "owner")
	exp := now.Add(time.Hour)
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsReadWrite, "owner", "d", &exp); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); !ok {
		t.Fatal("an unexpired grant is inactive")
	}
	h.clock.t = exp
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); ok {
		t.Error("a grant at its expiry is still active")
	}
	if has, _ := h.s.HasAnyWriteGrant(ctx, "exec"); has {
		t.Error("an expired grant still counts as a write grant")
	}
	if set, _ := h.s.GrantedSpaceIDs(ctx, "exec"); len(set) != 0 {
		t.Errorf("an expired grant still lists its space: %v", set)
	}
	// Revoking it is "nothing to revoke", not a fresh revocation.
	if _, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev"); !errors.Is(err, store.ErrNoGrant) {
		t.Errorf("revoking an expired grant = %v, want ErrNoGrant", err)
	}
	if n := len(eventsOfType(t, h.log, events.TypeSpaceAccessRevoked)); n != 0 {
		t.Errorf("revoking an expired grant emitted %d revocation event(s), want 0", n)
	}
}
