package deviceauth_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// The recovery encryption PUBLIC key registered at EnrolUser round-trips through
// the store: it is what LookupUser and ListUsers return, it is the recipient the
// device-enrolment response later carries (§41; rarebit-one/heyarr-mobile#41),
// and a user pinned without one simply has none.
func TestEnrolUserStoresRecoveryEncryptionKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	a := newActor(t)

	recovPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPub := encryption.FormatPublicKey(recovPriv.PublicKey().Bytes())

	user, err := f.store.EnrolUser(ctx, a.userKey, "alice", recoveryPub)
	if err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if user.RecoveryEncryptionKey != recoveryPub {
		t.Fatalf("EnrolUser returned recovery key %q, want %q", user.RecoveryEncryptionKey, recoveryPub)
	}
	got, err := f.store.LookupUser(ctx, a.userKey)
	if err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if got.RecoveryEncryptionKey != recoveryPub {
		t.Fatalf("LookupUser recovery key = %q, want %q", got.RecoveryEncryptionKey, recoveryPub)
	}
	users, err := f.store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || users[0].RecoveryEncryptionKey != recoveryPub {
		t.Fatalf("ListUsers recovery key = %+v, want one user with %q", users, recoveryPub)
	}
}

// A user pinned with no recovery key carries an empty one (the pre-recovery-wrap
// case), and a non-empty value that is not a well-formed x25519 public key is
// refused rather than stored.
func TestEnrolUserRecoveryKeyEmptyAndMalformed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	a := newActor(t)
	user, err := f.store.EnrolUser(ctx, a.userKey, "alice", "")
	if err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if user.RecoveryEncryptionKey != "" {
		t.Fatalf("recovery key = %q, want empty", user.RecoveryEncryptionKey)
	}

	f2 := newFixture(t)
	b := newActor(t)
	if _, err := f2.store.EnrolUser(ctx, b.userKey, "bob", "ed25519:deadbeef"); !errors.Is(err, deviceauth.ErrMalformedKey) {
		t.Fatalf("malformed recovery key error = %v, want ErrMalformedKey", err)
	}
}
