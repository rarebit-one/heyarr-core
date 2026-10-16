package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

const (
	encKeyA = "x25519:aaaa111111111111111111111111111111111111111111111111111111111111"
	encKeyB = "x25519:bbbb111111111111111111111111111111111111111111111111111111111111"
	encKeyC = "x25519:cccc111111111111111111111111111111111111111111111111111111111111"
)

// enrolDeviceWithKey enrols a fresh device for the user behind a, carrying the
// given encryption key ("" for a v1 cert with none), and returns its device key.
func enrolDeviceWithKey(t *testing.T, f *fixture, a *actor, encKey, name string) string {
	t.Helper()
	dpub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(a.userPriv, dpub, encKey, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.store.EnrolDevice(context.Background(), cert, name)
	if err != nil {
		t.Fatal(err)
	}
	return d.DeviceKey
}

// ResolveUser names a pinned user three ways (ADR-0108's member reference);
// DeviceWrapRecipients is their enrolled devices' encryption keys, by user —
// without revoked devices and without devices that carry no key.
func TestResolveUserAndDeviceWrapRecipients(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	alice, bob := newActor(t), newActor(t)
	ua, err := f.store.EnrolUser(ctx, alice.userKey, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	ub, err := f.store.EnrolUser(ctx, bob.userKey, "bob", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{ua.PrincipalID, ua.ID, "alice"} {
		got, err := f.store.ResolveUser(ctx, ref)
		if err != nil || got.PrincipalID != ua.PrincipalID || got.Name != "alice" {
			t.Errorf("ResolveUser(%q) = %+v, %v; want alice", ref, got, err)
		}
	}
	if _, err := f.store.ResolveUser(ctx, "nobody"); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Errorf("an unknown ref: %v", err)
	}
	if _, err := f.store.ResolveUser(ctx, ""); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Errorf("an empty ref: %v", err)
	}

	enrolDeviceWithKey(t, f, alice, encKeyA, "phone")
	revoked := enrolDeviceWithKey(t, f, alice, encKeyC, "old laptop")
	enrolDeviceWithKey(t, f, alice, "", "v1 device")
	enrolDeviceWithKey(t, f, bob, encKeyB, "phone")
	if _, err := f.store.RevokeDevice(ctx, revoked); err != nil {
		t.Fatal(err)
	}

	got, err := f.store.DeviceWrapRecipients(ctx, []string{ua.PrincipalID, ub.PrincipalID, "p-nobody"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{encKeyA: ua.PrincipalID, encKeyB: ub.PrincipalID}
	if len(got) != len(want) {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("recipients[%s] = %q, want %q", k, got[k], v)
		}
	}
	only, err := f.store.DeviceWrapRecipients(ctx, []string{ub.PrincipalID})
	if err != nil || len(only) != 1 || only[encKeyB] != ub.PrincipalID {
		t.Errorf("bob's recipients = %v, %v", only, err)
	}
	none, err := f.store.DeviceWrapRecipients(ctx, nil)
	if err != nil || len(none) != 0 {
		t.Errorf("no principals = %v, %v", none, err)
	}
}
