package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

var (
	execKey  = "x25519:" + strings.Repeat("ab", 32)
	ownerKey = "x25519:" + strings.Repeat("cd", 32)
	strayKey = "x25519:" + strings.Repeat("ef", 32)
)

// spaceWithOwnerWrap is an owned space holding the owner device's copy at
// epoch 0, as `space create` leaves it.
func (h *grantHarness) spaceWithOwnerWrap(t *testing.T) string {
	t.Helper()
	sp := h.space(t, "owner")
	if _, err := h.s.PutWrappedKey(context.Background(), sp, ownerKey, []byte("owner copy"), 0); err != nil {
		t.Fatal(err)
	}
	return sp
}

func (h *grantHarness) register(t *testing.T, principal, key string) store.ServiceRecipient {
	t.Helper()
	sr, _, err := h.s.RegisterServiceRecipient(context.Background(), principal, key, "home executor", "FP", "owner", "ed25519:dev")
	if err != nil {
		t.Fatal(err)
	}
	return sr
}

func recipientsOf(t *testing.T, s *store.Store, sp string) map[string]int {
	t.Helper()
	keys, err := s.WrappedKeysFor(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, k := range keys {
		out[k.Recipient] = k.Epoch
	}
	return out
}

// Registration is idempotent for the same executor, refused for another, and
// an event naming the device.
func TestRegisteringAServiceRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sr, created, err := h.s.RegisterServiceRecipient(ctx, "exec", execKey, "home executor", "FP", "owner", "ed25519:dev")
	if err != nil || !created {
		t.Fatalf("first registration: created=%v err=%v", created, err)
	}
	again, created, err := h.s.RegisterServiceRecipient(ctx, "exec", execKey, "renamed", "FP", "owner", "ed25519:dev")
	if err != nil || created || again.ID != sr.ID {
		t.Errorf("a repeated registration: %+v created=%v err=%v, want the first one unchanged", again, created, err)
	}
	if _, _, err := h.s.RegisterServiceRecipient(ctx, "exec2", execKey, "", "FP", "owner", "ed25519:dev"); !errors.Is(err, store.ErrRecipientTaken) {
		t.Errorf("the same key for another executor: %v, want ErrRecipientTaken", err)
	}
	own, err := h.s.RecipientsOf(ctx, "exec")
	if err != nil || !own[execKey] || len(own) != 1 {
		t.Errorf("RecipientsOf(exec) = %v, %v", own, err)
	}
	if evs := eventsOfType(t, h.log, events.TypeServiceRecipientRegistered); len(evs) != 1 {
		t.Errorf("%d registration events, want 1", len(evs))
	}
}

// A grant and its wraps land together or not at all: a wrap for a key that is
// not the executor's registered recipient, or at a superseded epoch, leaves no
// grant and no wrap behind.
func TestAGrantAndItsWrapAreOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)

	for name, w := range map[string]store.GrantWrap{
		"unregistered key": {Recipient: strayKey, Wrapped: []byte("w"), Epoch: 0},
		"future epoch":     {Recipient: execKey, Wrapped: []byte("w"), Epoch: 1},
		"empty wrap":       {Recipient: execKey, Epoch: 0},
	} {
		_, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil, w)
		if err == nil {
			t.Errorf("%s: the grant was accepted", name)
			continue
		}
		if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); ok {
			t.Errorf("%s: a refused grant left a grant behind", name)
		}
		if got := recipientsOf(t, h.s, sp); len(got) != 1 {
			t.Errorf("%s: a refused grant left wraps %v behind", name, got)
		}
	}
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
		store.GrantWrap{Recipient: strayKey, Wrapped: []byte("w")}); !errors.Is(err, store.ErrNotServiceRecipient) {
		t.Errorf("a wrap for an unregistered key: %v, want ErrNotServiceRecipient", err)
	}

	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
		store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); !ok {
		t.Error("the grant did not land")
	}
	if got := recipientsOf(t, h.s, sp); got[execKey] != 0 || len(got) != 2 {
		t.Errorf("wraps after the grant = %v, want the owner's and the executor's at epoch 0", got)
	}
	allowed, err := h.s.GrantedServiceRecipients(ctx, sp)
	if err != nil || !allowed[execKey] {
		t.Errorf("GrantedServiceRecipients = %v, %v; want the executor's key", allowed, err)
	}
}

// Revoking the grant deletes the executor's copy in the same step, and an
// expired grant stops the key being a wrap target. Removing the registration
// deletes its copies on every space.
func TestRevocationClosesBothGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	a, b := h.spaceWithOwnerWrap(t), h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)
	for _, sp := range []string{a, b} {
		if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
			store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); err != nil {
			t.Fatal(err)
		}
	}

	dropped, err := h.s.RevokeAccess(ctx, a, "exec", "owner", "ed25519:dev")
	if err != nil || len(dropped) != 1 || dropped[0] != execKey {
		t.Fatalf("revoke: dropped=%v err=%v", dropped, err)
	}
	if _, held := recipientsOf(t, h.s, a)[execKey]; held {
		t.Error("the revoked executor's copy survived the revoke")
	}
	if allowed, _ := h.s.GrantedServiceRecipients(ctx, a); allowed[execKey] {
		t.Error("a revoked executor's key is still a wrap target on the space")
	}
	if _, err := h.s.RevokeAccess(ctx, a, "exec", "owner", "ed25519:dev"); !errors.Is(err, store.ErrNoGrant) {
		t.Errorf("a second revoke: %v, want ErrNoGrant", err)
	}

	sr, spaces, err := h.s.RemoveServiceRecipient(ctx, execKey, "ed25519:dev")
	if err != nil || len(spaces) != 1 || spaces[0] != b {
		t.Fatalf("remove: %+v spaces=%v err=%v", sr, spaces, err)
	}
	if _, held := recipientsOf(t, h.s, b)[execKey]; held {
		t.Error("a removed recipient's copy survived on another space")
	}
	if own, _ := h.s.RecipientsOf(ctx, "exec"); len(own) != 0 {
		t.Errorf("a removed recipient is still the executor's: %v", own)
	}
	if _, _, err := h.s.RemoveServiceRecipient(ctx, sr.ID, "ed25519:dev"); !errors.Is(err, store.ErrUnknownRecipient) {
		t.Errorf("removing twice: %v, want ErrUnknownRecipient", err)
	}
	if evs := eventsOfType(t, h.log, events.TypeServiceRecipientRemoved); len(evs) != 1 {
		t.Errorf("%d removal events, want 1", len(evs))
	}
}

// A service recipient holds a copy of the current key, so a rotation's
// recipient compare-and-swap (#703) counts it: leaving it out is refused, and
// it is either re-wrapped or explicitly revoked.
func TestRotationAccountsForServiceRecipients(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
		store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); err != nil {
		t.Fatal(err)
	}

	ownerOnly := []store.RecipientWrap{{Recipient: ownerKey, Wrapped: []byte("owner e1")}}
	if _, err := h.s.RotateKey(ctx, sp, 0, []byte("sealed"), ownerOnly, []string{}, nil); !errors.Is(err, store.ErrRotationRecipientsChanged) {
		t.Fatalf("a rotation that forgot the executor: %v, want ErrRotationRecipientsChanged", err)
	}
	both := append(ownerOnly, store.RecipientWrap{Recipient: execKey, Wrapped: []byte("exec e1")})
	if _, err := h.s.RotateKey(ctx, sp, 0, []byte("sealed"), both, []string{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := recipientsOf(t, h.s, sp); got[execKey] != 1 {
		t.Errorf("after re-wrapping the executor: %v, want its copy at epoch 1", got)
	}
	if _, err := h.s.RotateKey(ctx, sp, 1, []byte("sealed"), ownerOnly, []string{execKey}, nil); err != nil {
		t.Fatal(err)
	}
	if _, held := recipientsOf(t, h.s, sp)[execKey]; held {
		t.Error("a rotation revoking the executor left it a copy")
	}
	// The grant survives a rotation: fetch and decrypt are separate gates.
	if _, ok, _ := h.s.ActiveGrant(ctx, sp, "exec"); !ok {
		t.Error("a rotation revoked the grant")
	}
	// A wrap at the old epoch is refused for a grant now.
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
		store.GrantWrap{Recipient: execKey, Wrapped: []byte("stale"), Epoch: 1}); !errors.Is(err, store.ErrStaleKeyEpoch) {
		t.Errorf("a grant wrapping a superseded epoch: %v, want ErrStaleKeyEpoch", err)
	}
}

// An expired grant drops its executor's key from the space's wrap targets.
func TestAnExpiredGrantIsNoWrapTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)
	exp := now.Add(1)
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", &exp,
		store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := h.s.GrantedServiceRecipients(ctx, sp); !allowed[execKey] {
		t.Fatal("an active grant's key is not a wrap target")
	}
	h.clock.t = exp
	if allowed, _ := h.s.GrantedServiceRecipients(ctx, sp); allowed[execKey] {
		t.Error("an expired grant's key is still a wrap target")
	}
	// Revoking the expired grant is no fresh revocation, but its copy still goes.
	dropped, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev")
	if err != nil || len(dropped) != 1 || dropped[0] != execKey {
		t.Errorf("revoking an expired grant with a copy: dropped=%v err=%v", dropped, err)
	}
	if n := len(eventsOfType(t, h.log, events.TypeSpaceAccessRevoked)); n != 0 {
		t.Errorf("revoking an expired grant emitted %d revocation event(s), want 0", n)
	}
	if _, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev"); !errors.Is(err, store.ErrNoGrant) {
		t.Errorf("revoking again: %v, want ErrNoGrant", err)
	}
}

// The service-recipient check runs inside the transaction that writes the wrap
// (review P1): a revoke or an expiry committing after an earlier, separate
// check — the API's — cannot be undone by the write that follows it. Here the
// revoke lands, then the store write the API would have made is attempted.
func TestARevokeBetweenCheckAndWriteRecreatesNoWrap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)
	if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", nil,
		store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); err != nil {
		t.Fatal(err)
	}
	// The API's check would pass here.
	if allowed, _ := h.s.GrantedServiceRecipients(ctx, sp); !allowed[execKey] {
		t.Fatal("precondition: the executor's key is a wrap target")
	}
	if _, err := h.s.RevokeAccess(ctx, sp, "exec", "owner", "ed25519:dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.PutWrappedKey(ctx, sp, execKey, []byte("recreated"), 0); !errors.Is(err, store.ErrServiceRecipientUngranted) {
		t.Errorf("a re-wrap after the revoke: %v, want ErrServiceRecipientUngranted", err)
	}
	both := []store.RecipientWrap{
		{Recipient: ownerKey, Wrapped: []byte("owner e1")},
		{Recipient: execKey, Wrapped: []byte("exec e1")},
	}
	// A rotation re-wrapping it is refused on the grant, before the recipient
	// compare-and-swap would refuse it for holding no copy.
	if _, err := h.s.RotateKey(ctx, sp, 0, []byte("sealed"), both, []string{}, nil); !errors.Is(err, store.ErrServiceRecipientUngranted) {
		t.Errorf("a rotation re-wrapping the revoked executor: %v, want ErrServiceRecipientUngranted", err)
	}
	if _, held := recipientsOf(t, h.s, sp)[execKey]; held {
		t.Error("a revoked executor's copy was recreated")
	}

	// The same holds for a grant that expires between the check and the write.
	other := h.spaceWithOwnerWrap(t)
	exp := now.Add(time.Minute)
	if _, err := h.s.GrantAccess(ctx, other, "exec", store.CapsRead, "owner", "ed25519:dev", &exp); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.PutWrappedKey(ctx, other, execKey, []byte("exec copy"), 0); err != nil {
		t.Fatal(err)
	}
	h.clock.t = exp
	if _, err := h.s.PutWrappedKey(ctx, other, execKey, []byte("late"), 0); !errors.Is(err, store.ErrServiceRecipientUngranted) {
		t.Errorf("a re-wrap after the grant expired: %v, want ErrServiceRecipientUngranted", err)
	}
	// It still holds its old copy; a rotation may not carry it forward, only
	// revoke it.
	keep := []store.RecipientWrap{
		{Recipient: ownerKey, Wrapped: []byte("owner e1")},
		{Recipient: execKey, Wrapped: []byte("exec e1")},
	}
	if _, err := h.s.RotateKey(ctx, other, 0, []byte("sealed"), keep, []string{}, nil); !errors.Is(err, store.ErrServiceRecipientUngranted) {
		t.Errorf("a rotation carrying an expired executor forward: %v, want ErrServiceRecipientUngranted", err)
	}
	if _, err := h.s.RotateKey(ctx, other, 0, []byte("sealed"), keep[:1], []string{execKey}, nil); err != nil {
		t.Errorf("a rotation revoking the expired executor: %v", err)
	}
	if _, held := recipientsOf(t, h.s, other)[execKey]; held {
		t.Error("an expired executor kept a copy past the rotation")
	}
}

// A grant whose expiry is not in the future is refused before anything is
// written (review P2): it would be inactive yet carry a live wrap.
func TestAGrantThatHasAlreadyExpiredIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	sp := h.spaceWithOwnerWrap(t)
	h.register(t, "exec", execKey)
	for _, exp := range []time.Time{now, now.Add(-time.Hour)} {
		if _, err := h.s.GrantAccess(ctx, sp, "exec", store.CapsRead, "owner", "ed25519:dev", &exp,
			store.GrantWrap{Recipient: execKey, Wrapped: []byte("exec copy")}); !errors.Is(err, store.ErrInvalidExpiry) {
			t.Errorf("a grant expiring at %v: %v, want ErrInvalidExpiry", exp, err)
		}
	}
	if _, held := recipientsOf(t, h.s, sp)[execKey]; held {
		t.Error("a refused grant wrote a wrap")
	}
	if n := len(eventsOfType(t, h.log, events.TypeSpaceAccessGranted)); n != 0 {
		t.Errorf("a refused grant emitted %d grant event(s)", n)
	}
}

// A member's key is never registered as an executor's (review P2): a non-revoked
// device's encryption key or a recovery key is refused, so removing a service
// recipient can only ever delete the executor's own wraps. (deviceauth refuses
// the reverse direction; see its tests.)
func TestAMemberKeyIsNeverAServiceRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t)
	ts := now.Format(time.RFC3339Nano)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO user_identities (id, principal_id, public_key, recovery_encryption_key, enrolled_at) VALUES ('u1', 'owner', 'ed25519:u', ?, ?)`, []any{ownerKey, ts}},
		{`INSERT INTO device_identities (id, user_id, device_key, encryption_key, name, cert, enrolled_at, expires_at)
		  VALUES ('d1', 'u1', 'ed25519:d', ?, '', 'c', ?, ?)`, []any{strayKey, ts, now.Add(time.Hour).Format(time.RFC3339Nano)}},
	} {
		if _, err := h.db.Writer().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{ownerKey, strayKey} {
		if _, _, err := h.s.RegisterServiceRecipient(ctx, "exec", key, "", "FP", "owner", "d"); !errors.Is(err, store.ErrRecipientIsMemberKey) {
			t.Errorf("registering member key %s: %v, want ErrRecipientIsMemberKey", key, err)
		}
	}
	// A revoked device's key is no longer a member's.
	if _, err := h.db.Writer().ExecContext(ctx, `UPDATE device_identities SET revoked_at = ? WHERE id = 'd1'`, ts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.RegisterServiceRecipient(ctx, "exec", strayKey, "", "FP", "owner", "d"); err != nil {
		t.Errorf("registering a revoked device's former key: %v", err)
	}
}
