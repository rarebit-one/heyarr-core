package deviceauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

const (
	oldRecovery = "x25519:1111111111111111111111111111111111111111111111111111111111111111"
	newRecovery = "x25519:2222222222222222222222222222222222222222222222222222222222222222"
)

// newUserKey is a fresh, well-formed user public key.
func newUserKey(t *testing.T) string {
	t.Helper()
	u, _, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return u.UserID()
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The cutover's one write: the key and recovery key change in place, the
// principal and its devices survive, and the old key's op log is dropped.
func TestRekeyUserReplacesTheKeyInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	a := newActor(t)
	before, err := f.store.EnrolUser(ctx, a.userKey, "alice", oldRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnrolDevice(ctx, a.cert, "phone"); err != nil {
		t.Fatal(err)
	}
	ops := f.count(t, `SELECT count(*) FROM membership_ops WHERE user_id = ?`, before.ID)
	if ops == 0 {
		t.Fatal("fixture recorded no membership ops; the deletion would be untested")
	}

	newKey := newUserKey(t)
	rk, err := f.store.RekeyUser(ctx, before.PrincipalID, newKey, newRecovery)
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if rk.User.ID != before.ID || rk.User.PrincipalID != before.PrincipalID || rk.User.Name != "alice" {
		t.Errorf("identity changed: before %+v, after %+v", before, rk.User)
	}
	if rk.OldPublicKey != a.userKey || rk.User.PublicKey != newKey {
		t.Errorf("public key %s -> %s, want %s -> %s", rk.OldPublicKey, rk.User.PublicKey, a.userKey, newKey)
	}
	if rk.OldRecoveryEncryptionKey != oldRecovery || rk.User.RecoveryEncryptionKey != newRecovery {
		t.Errorf("recovery key %s -> %s", rk.OldRecoveryEncryptionKey, rk.User.RecoveryEncryptionKey)
	}
	if rk.OpsDeleted != ops {
		t.Errorf("ops deleted = %d, want %d", rk.OpsDeleted, ops)
	}

	// Both columns changed on the same row.
	got, err := f.store.LookupUser(ctx, newKey)
	if err != nil {
		t.Fatalf("new key is not pinned: %v", err)
	}
	if got.ID != before.ID || got.PrincipalID != before.PrincipalID || got.RecoveryEncryptionKey != newRecovery {
		t.Errorf("pinned row = %+v", got)
	}
	if _, err := f.store.LookupUser(ctx, a.userKey); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Errorf("old key still resolves: %v", err)
	}

	// The old key's ops are gone; the device row is not (RevokeUser's cascade
	// would have taken it), and it is not revoked either.
	if n := f.count(t, `SELECT count(*) FROM membership_ops WHERE user_id = ?`, before.ID); n != 0 {
		t.Errorf("%d membership ops survived", n)
	}
	dev, err := f.store.LookupDevice(ctx, a.deviceKey)
	if err != nil {
		t.Fatalf("device row did not survive: %v", err)
	}
	if dev.UserID != before.ID || dev.RevokedAt != nil {
		t.Errorf("device row changed: %+v", dev)
	}
	if n := f.count(t, `SELECT count(*) FROM events WHERE type = ?`, events.TypeUserRevoked); n != 0 {
		t.Errorf("a rekey emitted %d identity.user.revoked", n)
	}

	// The old device no longer authenticates: its cert names the old key.
	if _, err := f.store.Verify(ctx, a.credential(t, now), nil, now); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Errorf("old-key device verified after rekey: %v", err)
	}

	// The event names both keys and the count, and nothing else.
	var payload string
	if err := f.db.Reader().QueryRow(
		`SELECT payload FROM events WHERE type = ? AND subject_type = 'user_identity' AND subject_id = ?`,
		events.TypeUserRekeyed, before.ID).Scan(&payload); err != nil {
		t.Fatalf("no %s event: %v", events.TypeUserRekeyed, err)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"principal_id":                before.PrincipalID,
		"old_public_key":              a.userKey,
		"public_key":                  newKey,
		"old_recovery_encryption_key": oldRecovery,
		"recovery_encryption_key":     newRecovery,
		"ops_deleted":                 float64(ops),
	}
	if len(ev) != len(want) {
		t.Errorf("event payload = %v", ev)
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("event %s = %v, want %v", k, ev[k], v)
		}
	}

	// A restart's legacy-cert backfill must not choke on the surviving device
	// row, whose cert names the old key.
	if n, err := f.store.BackfillLegacyCerts(ctx); err != nil || n != 0 {
		t.Errorf("backfill after rekey = %d, %v; want 0, nil", n, err)
	}
}

// A principal resolves by principal id, user identity id or name.
func TestRekeyUserResolvesThePrincipal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, by := range []string{"principal id", "user identity id", "name"} {
		t.Run(by, func(t *testing.T) {
			f := newFixture(t)
			u, err := f.store.EnrolUser(ctx, newUserKey(t), "alice", "")
			if err != nil {
				t.Fatal(err)
			}
			ref := map[string]string{"principal id": u.PrincipalID, "user identity id": u.ID, "name": "alice"}[by]
			rk, err := f.store.RekeyUser(ctx, ref, newUserKey(t), newRecovery)
			if err != nil {
				t.Fatalf("rekey by %s: %v", by, err)
			}
			if rk.User.ID != u.ID || rk.OpsDeleted != 0 || rk.OldRecoveryEncryptionKey != "" {
				t.Errorf("rekey by %s = %+v", by, rk)
			}
		})
	}
}

func TestRekeyUserRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type setup struct {
		f          *fixture
		alice, bob deviceauth.User
	}
	newSetup := func(t *testing.T) setup {
		f := newFixture(t)
		alice, err := f.store.EnrolUser(ctx, newUserKey(t), "alice", oldRecovery)
		if err != nil {
			t.Fatal(err)
		}
		// bob's NAME is alice's principal id, so that reference is ambiguous.
		bob, err := f.store.EnrolUser(ctx, newUserKey(t), alice.PrincipalID, "")
		if err != nil {
			t.Fatal(err)
		}
		return setup{f: f, alice: alice, bob: bob}
	}

	tests := []struct {
		name string
		args func(t *testing.T, s setup) (principal, key, recovery string)
		want error
	}{
		{"unknown principal", func(t *testing.T, s setup) (string, string, string) {
			return "carol", newUserKey(t), newRecovery
		}, deviceauth.ErrUnknownUser},
		{"empty principal", func(t *testing.T, s setup) (string, string, string) {
			return "", newUserKey(t), newRecovery
		}, deviceauth.ErrUnknownUser},
		{"ambiguous principal", func(t *testing.T, s setup) (string, string, string) {
			return s.alice.PrincipalID, newUserKey(t), newRecovery
		}, deviceauth.ErrAmbiguousUser},
		{"malformed public key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", "ed25519:nothex", newRecovery
		}, deviceauth.ErrMalformedKey},
		{"recovery key given as public key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", newRecovery, newRecovery
		}, deviceauth.ErrMalformedKey},
		{"malformed recovery key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", newUserKey(t), "x25519:abcd"
		}, deviceauth.ErrMalformedKey},
		{"public key given as recovery key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", newUserKey(t), newUserKey(t)
		}, deviceauth.ErrMalformedKey},
		{"missing recovery key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", newUserKey(t), ""
		}, deviceauth.ErrMalformedKey},
		{"key pinned to another user", func(t *testing.T, s setup) (string, string, string) {
			return "alice", s.bob.PublicKey, newRecovery
		}, deviceauth.ErrUserExists},
		{"same key", func(t *testing.T, s setup) (string, string, string) {
			return "alice", s.alice.PublicKey, newRecovery
		}, deviceauth.ErrSameKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSetup(t)
			principal, key, recovery := tt.args(t, s)
			if _, err := s.f.store.RekeyUser(ctx, principal, key, recovery); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			// Nothing changed and nothing was recorded.
			got, err := s.f.store.LookupUser(ctx, s.alice.PublicKey)
			if err != nil || got.RecoveryEncryptionKey != oldRecovery {
				t.Errorf("alice changed after a refusal: %+v, %v", got, err)
			}
			if n := s.f.count(t, `SELECT count(*) FROM events WHERE type = ?`, events.TypeUserRekeyed); n != 0 {
				t.Errorf("a refused rekey emitted %d events", n)
			}
		})
	}
}
