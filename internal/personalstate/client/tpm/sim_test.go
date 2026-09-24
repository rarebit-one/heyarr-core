//go:build tpmsim

// This file is the LIVE seal/unseal proof, run behind the `tpmsim` build tag
// against the in-process TPM 2.0 reference simulator (go-tpm-tools, cgo). The tag
// keeps it — and the shared round-trip body below — out of the default
// CGO_ENABLED=0 matrix (which cannot build the cgo simulator, and where the body
// would otherwise read as unused). A dedicated CI step runs it with
// CGO_ENABLED=1, and a developer can run it locally the same way:
//
//	CGO_ENABLED=1 go test -tags tpmsim ./internal/personalstate/client/tpm/
//
// go-tpm's transport API targets this reference simulator; swtpm's socket control
// channel speaks a different protocol and is not a drop-in here.
package tpm

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/google/go-tpm-tools/simulator"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/rarebit-one/voidbind-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

func TestSealUnsealAgainstSimulator(t *testing.T) {
	sim, err := simulator.Get()
	if err != nil {
		t.Fatalf("simulator: %v", err)
	}
	tpm := transport.FromReadWriteCloser(sim)
	defer func() { _ = tpm.Close() }()
	sealUnsealCanary(t, tpm)
}

// TestSealToFileRoundTrips proves the provisioning artifact: a sealed key written
// to disk (Blob.WriteFile, as `heyarr device seal-tpm` does) reloads
// (ReadBlobFile) into a backend that unwraps a space wrapped to it.
func TestSealToFileRoundTrips(t *testing.T) {
	sim, err := simulator.Get()
	if err != nil {
		t.Fatalf("simulator: %v", err)
	}
	tpm := transport.FromReadWriteCloser(sim)
	defer func() { _ = tpm.Close() }()

	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	blob, err := Seal(tpm, seed, []byte("123456"), PCRSelection(7))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "nested", "device.sealed")
	if err := blob.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadBlobFile(path)
	if err != nil {
		t.Fatalf("ReadBlobFile: %v", err)
	}

	sk, _ := encryption.NewSpaceKey()
	pub, _ := encryption.ParsePublicKey(encryption.FormatPublicKey(got.Public))
	wrapped, _ := encryption.Seal(sk, pub)
	u, err := New(got, func() (string, error) { return "123456", nil }, noCloseOpener(tpm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(wrapped); err != nil {
		t.Fatalf("unwrap with the reloaded sealed key: %v", err)
	}
}

// sealUnsealCanary seals a fresh X25519 seed to the given TPM, round-trips the
// blob through disk form, wraps a canary space key to the recorded public point
// (the controller's job), opens it through the backend, proves the recovered key
// decrypts the canary, and proves a wrong PIN is refused.
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
// calls (which each Close their handle) — the test manages the connection.
func noCloseOpener(t transport.TPM) Opener {
	return func() (transport.TPMCloser, error) { return noClose{t}, nil }
}

type noClose struct{ transport.TPM }

func (noClose) Close() error { return nil }
