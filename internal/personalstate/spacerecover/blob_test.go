package spacerecover_test

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/identity"
	"github.com/rarebit-one/voidbind-go/recovery"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
)

func mustSecret(t *testing.T) recovery.Secret {
	t.Helper()
	s, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func recipientOf(t *testing.T, s recovery.Secret) string {
	t.Helper()
	id, err := spacerecover.RecipientID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func userOf(s recovery.Secret) string {
	pub := ed25519.NewKeyFromSeed(recovery.DeriveUserSeed(s)).Public().(ed25519.PublicKey)
	return identity.FormatPublicKey(pub)
}

// TestBlobRoundTrip: a blob sealed with only the recovery PUBLIC key opens with
// the secret, and the keys it carries unwrap to the originals.
func TestBlobRoundTrip(t *testing.T) {
	secret := mustSecret(t)
	skA, wA := sealForRecovery(t, secret)
	skB, wB := sealForRecovery(t, secret)
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	data, err := spacerecover.SealBlob(spacerecover.Blob{
		UserID:            userOf(secret),
		RecoveryRecipient: recipientOf(t, secret),
		GeneratedAt:       at,
		Spaces: []spacerecover.BlobSpace{
			{SpaceID: "space-b", Kind: "family", Wrapped: wB},
			{SpaceID: "space-a", Kind: "personal", Wrapped: wA},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte(spacerecover.BlobFormat+"\x00")) {
		t.Fatal("the file does not start with its format")
	}
	if bytes.Contains(data, []byte("space-a")) || bytes.Contains(data, wA) {
		t.Fatal("the blob leaks its contents at rest")
	}

	b, err := spacerecover.OpenBlob(secret, data)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	if b.Format != spacerecover.BlobFormat || !b.GeneratedAt.Equal(at) || len(b.Spaces) != 2 || b.Spaces[0].SpaceID != "space-a" {
		t.Fatalf("opened blob = %+v", b)
	}
	keys, err := spacerecover.UnwrapAll(secret, b.Wrapped())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]encryption.SpaceKey{"space-a": skA, "space-b": skB} {
		ct, err := encryption.EncryptChange(want, []byte("canary"))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := encryption.DecryptChange(keys[id], ct); err != nil || string(got) != "canary" {
			t.Fatalf("%s: recovered key does not open the original's ciphertext", id)
		}
	}
}

// TestBlobRefusesTheWrongSecretAndDamage: another secret does not open the blob,
// nor does a flipped byte or a non-blob file.
func TestBlobRefusesTheWrongSecretAndDamage(t *testing.T) {
	secret := mustSecret(t)
	_, w := sealForRecovery(t, secret)
	data, err := spacerecover.SealBlob(spacerecover.Blob{
		RecoveryRecipient: recipientOf(t, secret),
		Spaces:            []spacerecover.BlobSpace{{SpaceID: "s", Kind: "personal", Wrapped: w}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := spacerecover.OpenBlob(mustSecret(t), data); !errors.Is(err, spacerecover.ErrBlobSecret) {
		t.Fatalf("wrong secret: err = %v, want ErrBlobSecret", err)
	}
	damaged := bytes.Clone(data)
	damaged[len(damaged)-1] ^= 0x01
	if _, err := spacerecover.OpenBlob(secret, damaged); !errors.Is(err, spacerecover.ErrBlobSecret) {
		t.Fatalf("damaged: err = %v, want ErrBlobSecret", err)
	}
	for _, junk := range [][]byte{nil, []byte("not a blob"), []byte(spacerecover.BlobFormat + "\x00\xff\xff\xff\xff")} {
		if _, err := spacerecover.OpenBlob(secret, junk); !errors.Is(err, spacerecover.ErrNotABlob) {
			t.Fatalf("junk %q: err = %v, want ErrNotABlob", junk, err)
		}
	}
}

// TestBlobRefusesAMismatchedUser: a blob naming another user is refused even
// though it is sealed to this secret's recovery key.
func TestBlobRefusesAMismatchedUser(t *testing.T) {
	secret := mustSecret(t)
	data, err := spacerecover.SealBlob(spacerecover.Blob{
		UserID:            userOf(mustSecret(t)),
		RecoveryRecipient: recipientOf(t, secret),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spacerecover.OpenBlob(secret, data); err == nil {
		t.Fatal("a blob naming another user opened")
	}
}

// TestAnyoneCanMintABlob pins the property the re-wrap check exists for: the
// blob is sealed to a PUBLIC key, so a forger who knows it produces a blob the
// secret opens cleanly, carrying keys of the forger's choosing. Opening is not
// authentication.
func TestAnyoneCanMintABlob(t *testing.T) {
	secret := mustSecret(t)
	pub, err := encryption.ParsePublicKey(recipientOf(t, secret))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	w, err := encryption.Seal(forged, pub)
	if err != nil {
		t.Fatal(err)
	}
	data, err := spacerecover.SealBlob(spacerecover.Blob{
		RecoveryRecipient: recipientOf(t, secret),
		Spaces:            []spacerecover.BlobSpace{{SpaceID: "victim", Kind: "personal", Wrapped: w}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := spacerecover.OpenBlob(secret, data)
	if err != nil {
		t.Fatalf("a forged blob is indistinguishable at open time, and must open: %v", err)
	}
	if _, err := spacerecover.UnwrapAll(secret, b.Wrapped()); err != nil {
		t.Fatal(err)
	}
}
