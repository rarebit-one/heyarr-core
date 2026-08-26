package client_test

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// dev draws a device X25519 key, its recipient (rendered public key), and an
// Unwrapper over the exportable private key — the CLI stand-in.
func dev(t *testing.T) (*ecdh.PrivateKey, client.Recipient, client.Unwrapper) {
	t.Helper()
	priv, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.ParseRecipient(encryption.FormatPublicKey(priv.PublicKey().Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return priv, r, client.NewKeyUnwrapper(priv)
}

// wrappedFor returns the sealed copy for a given recipient id from a Create result.
func wrappedFor(t *testing.T, ws []client.WrappedFor, id string) []byte {
	t.Helper()
	for _, w := range ws {
		if w.Recipient == id {
			return w.Wrapped
		}
	}
	t.Fatalf("no wrapped key for recipient %s", id)
	return nil
}

// TestTwoDevicesReadOneSpace: device A creates a space for A and B; B opens its
// wrapped copy and reads a change A wrote — the multi-device read §41 exists for.
func TestTwoDevicesReadOneSpace(t *testing.T) {
	t.Parallel()
	_, ra, _ := dev(t)
	_, rb, ub := dev(t)

	a := client.New()
	sp, wrapped, err := a.Create(spaces.KindFamily, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(wrapped) != 2 {
		t.Fatalf("got %d wrapped copies, want 2", len(wrapped))
	}

	// A writes a change.
	plaintext := []byte("shared playlist entry")
	change, err := a.Encrypt(sp.ID, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// B opens its copy and reads it.
	b := client.New()
	if err := b.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	got, err := b.Decrypt(sp.ID, change)
	if err != nil {
		t.Fatalf("B.Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("B read a different plaintext than A wrote")
	}
}

// TestNonRecipientCannotOpen: a device the space was NOT wrapped for cannot open
// any of the wrapped copies — the confidentiality boundary.
func TestNonRecipientCannotOpen(t *testing.T) {
	t.Parallel()
	_, ra, _ := dev(t)
	_, _, uStranger := dev(t)

	a := client.New()
	sp, wrapped, err := a.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stranger := client.New()
	if err := stranger.Open(sp.ID, wrapped[0].Wrapped, uStranger); err == nil {
		t.Fatal("a non-recipient opened a wrapped key")
	}
	if stranger.IsOpen(sp.ID) {
		t.Fatal("a failed Open left the space marked open")
	}
}

// TestEncryptDecryptRequireOpenSpace: without the key held, encrypt and decrypt
// refuse rather than operating on a nil key.
func TestEncryptDecryptRequireOpenSpace(t *testing.T) {
	t.Parallel()
	m := client.New()
	if _, err := m.Encrypt("unopened", []byte("x")); !errors.Is(err, client.ErrSpaceNotOpen) {
		t.Fatalf("Encrypt on unopened = %v, want ErrSpaceNotOpen", err)
	}
	if _, err := m.Decrypt("unopened", []byte("x")); !errors.Is(err, client.ErrSpaceNotOpen) {
		t.Fatalf("Decrypt on unopened = %v, want ErrSpaceNotOpen", err)
	}
}

// TestCreateRequiresRecipients: a space with no recipients could be read by no
// one, so Create refuses it.
func TestCreateRequiresRecipients(t *testing.T) {
	t.Parallel()
	if _, _, err := client.New().Create(spaces.KindPersonal, testNow, nil); err == nil {
		t.Fatal("Create accepted a space with no recipients")
	}
}

// TestCloseForgetsTheKey: closing a space drops the in-memory key; the device can
// no longer read it (until re-opened), and the wrapped copies are untouched — the
// caller keeps them.
func TestCloseForgetsTheKey(t *testing.T) {
	t.Parallel()
	_, ra, ua := dev(t)
	m := client.New()
	sp, wrapped, err := m.Create(spaces.KindResearch, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	change, err := m.Encrypt(sp.ID, []byte("note"))
	if err != nil {
		t.Fatal(err)
	}

	m.Close(sp.ID)
	if m.IsOpen(sp.ID) {
		t.Fatal("space still open after Close")
	}
	if _, err := m.Encrypt(sp.ID, []byte("x")); !errors.Is(err, client.ErrSpaceNotOpen) {
		t.Fatal("a closed space still encrypts")
	}
	// Re-opening from the wrapped copy restores the ability to read prior changes.
	if err := m.Open(sp.ID, wrapped[0].Wrapped, ua); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	got, err := m.Decrypt(sp.ID, change)
	if err != nil || string(got) != "note" {
		t.Fatalf("re-opened space did not read the prior change: %v %q", err, got)
	}
}

// TestParseRecipientRejectsNonEncryptionKey: a recipient id must be an encryption
// key, not a signing key — the same primitive separation the codec enforces.
func TestParseRecipientRejectsNonEncryptionKey(t *testing.T) {
	t.Parallel()
	if _, err := client.ParseRecipient("ed25519:00"); err == nil {
		t.Fatal("ParseRecipient accepted an ed25519 key")
	}
	if _, err := client.ParseRecipient(""); err == nil {
		t.Fatal("ParseRecipient accepted an empty id")
	}
}

// TestEnclaveStyleUnwrapperWorks: an Unwrapper that never exposes a private key —
// the shape a phone's keystore takes (#330) — opens a space exactly as the
// exportable stand-in does. This proves the interface, not a raw key, is what the
// client depends on.
func TestEnclaveStyleUnwrapperWorks(t *testing.T) {
	t.Parallel()
	priv, ra, _ := dev(t)
	a := client.New()
	sp, wrapped, err := a.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	change, err := a.Encrypt(sp.ID, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	// enclave holds the key and does the ECDH "inside", never exposing priv.
	enclave := enclaveUnwrapper{priv: priv}
	b := client.New()
	if err := b.Open(sp.ID, wrapped[0].Wrapped, enclave); err != nil {
		t.Fatalf("enclave Open: %v", err)
	}
	got, err := b.Decrypt(sp.ID, change)
	if err != nil || string(got) != "secret" {
		t.Fatalf("enclave-opened space did not read the change: %v %q", err, got)
	}
}

// enclaveUnwrapper stands in for a keystore/secure-element unwrapper: it performs
// the unwrap without the caller ever holding the private key as an *ecdh key it
// could export — here it simply does not expose priv beyond this method.
type enclaveUnwrapper struct{ priv *ecdh.PrivateKey }

func (e enclaveUnwrapper) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	return encryption.Unwrap(wrapped, e.priv)
}
