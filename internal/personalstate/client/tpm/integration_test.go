package tpm

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/google/go-tpm/tpm2/transport"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// sealUnsealCanary is the reusable integration body: it seals a fresh X25519 seed
// to the given TPM, round-trips the blob through disk form, wraps a canary space
// key to the recorded public point (the controller's job), opens it through the
// backend, proves the recovered key decrypts the canary, and proves a wrong PIN
// is refused. It is driven against swtpm in CI (swtpm_test.go) and, locally, can
// be driven against the cgo simulator behind the `tpmsim` build tag.
func sealUnsealCanary(t *testing.T, tpm transport.TPM) {
	t.Helper()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	const pin = "123456"

	blob, err := Seal(tpm, seed, []byte(pin), PCRSelection(7))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The blob survives the on-disk round-trip unchanged.
	blob, err = UnmarshalBlob(blob.Marshal())
	if err != nil {
		t.Fatalf("blob round-trip: %v", err)
	}

	// Controller side: mint a space key, wrap it to the sealed key's public point,
	// and seal a canary under it.
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := encryption.ParsePublicKey(encryption.FormatPublicKey(blob.Public))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Seal(sk, pub)
	if err != nil {
		t.Fatal(err)
	}
	canary := make([]byte, 48)
	if _, err := rand.Read(canary); err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(sk, canary)
	if err != nil {
		t.Fatal(err)
	}

	// Device side: open through the backend, unsealing after the TPM gate.
	u, err := New(blob, func() (string, error) { return pin, nil }, noCloseOpener(tpm))
	if err != nil {
		t.Fatal(err)
	}
	var _ client.Custody = u
	if u.RecipientID() != encryption.FormatPublicKey(blob.Public) {
		t.Fatalf("RecipientID = %s, want the sealed public point", u.RecipientID())
	}
	key, err := u.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	got, err := encryption.DecryptChange(key, ct)
	if err != nil {
		t.Fatalf("decrypt canary with the TPM-recovered key: %v", err)
	}
	if !bytes.Equal(got, canary) {
		t.Fatal("the TPM-recovered key did not decrypt the canary")
	}

	// A wrong PIN is refused at the gate.
	bad, err := New(blob, func() (string, error) { return "999999", nil }, noCloseOpener(tpm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Unwrap(wrapped); err == nil {
		t.Fatal("SECURITY: Unwrap with the wrong PIN succeeded")
	}
}

// noCloseOpener yields an Opener that reuses one open transport across Unwrap
// calls (which each Close their handle) — for tests where the connection is
// managed by the test, not the backend.
func noCloseOpener(t transport.TPM) Opener {
	return func() (transport.TPMCloser, error) { return noClose{t}, nil }
}

type noClose struct{ transport.TPM }

func (noClose) Close() error { return nil }
