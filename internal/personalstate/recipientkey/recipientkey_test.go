package recipientkey_test

import (
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/recipientkey"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

func writePIN(t *testing.T, dir, name, pin string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(pin), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func recipientPub(t *testing.T, recipient string) *ecdh.PublicKey {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(recipient, servicerecipient.Prefix))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// The key is born sealed, its public half and fingerprint read back without
// the PIN, and a space key wrapped for it opens only through the seal.
func TestSealedKeyRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pin := recipientkey.FilePIN(writePIN(t, dir, "pin", "correct horse\n", 0o600))
	path := filepath.Join(dir, "keys", "recipient.sealed")

	made, err := recipientkey.Init(path, pin)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := servicerecipient.Fingerprint(made.Recipient)
	if err != nil || made.Fingerprint != fp {
		t.Fatalf("Init fingerprint = %q, want %q (%v)", made.Fingerprint, fp, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the sealed file is %v, want 0600", fi.Mode().Perm())
	}

	shown, err := recipientkey.Show(path)
	if err != nil {
		t.Fatal(err)
	}
	if shown != made {
		t.Errorf("Show = %+v, want what Init printed, %+v", shown, made)
	}

	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Seal(sk, recipientPub(t, made.Recipient))
	if err != nil {
		t.Fatal(err)
	}
	h, closeKey, err := recipientkey.Open(path, pin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeKey()
	if h.RecipientID() != made.Recipient {
		t.Errorf("RecipientID = %s, want %s", h.RecipientID(), made.Recipient)
	}
	got, err := h.Unwrap(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(sk, []byte("canary"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := encryption.DecryptChange(got, ct); err != nil || string(pt) != "canary" {
		t.Errorf("the unwrapped space key is not the one wrapped: %q, %v", pt, err)
	}

	// A second init never replaces the key that spaces are wrapped for.
	if _, err := recipientkey.Init(path, pin); err == nil {
		t.Error("Init replaced an existing sealed key")
	}
	again, err := recipientkey.Show(path)
	if err != nil || again.Recipient != made.Recipient {
		t.Errorf("after a refused re-init the key is %+v (%v)", again, err)
	}
}

func TestOpenFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "recipient.sealed")
	if _, err := recipientkey.Init(path, recipientkey.FilePIN(writePIN(t, dir, "pin", "right", 0o600))); err != nil {
		t.Fatal(err)
	}

	if _, _, err := recipientkey.Open(path, recipientkey.FilePIN(writePIN(t, dir, "wrong", "wrong", 0o600)), time.Minute); err == nil {
		t.Error("a wrong PIN opened the sealed key")
	}
	if _, _, err := recipientkey.Open(filepath.Join(dir, "absent.sealed"), recipientkey.FilePIN(filepath.Join(dir, "pin")), time.Minute); !errors.Is(err, recipientkey.ErrNoSealedFile) {
		t.Errorf("no sealed file: got %v, want ErrNoSealedFile", err)
	}
	if _, err := recipientkey.Show(filepath.Join(dir, "absent.sealed")); !errors.Is(err, recipientkey.ErrNoSealedFile) {
		t.Errorf("Show with no sealed file: got %v, want ErrNoSealedFile", err)
	}
	if _, _, err := recipientkey.Open(path, recipientkey.FilePIN(filepath.Join(dir, "no-such-pin")), time.Minute); err == nil {
		t.Error("an absent PIN file opened the sealed key")
	}
}

// The PIN sources refuse rather than fall back.
func TestPINSources(t *testing.T) {
	dir := t.TempDir()
	good := writePIN(t, dir, recipientkey.DefaultPINCredential, "s3cret\r\n", 0o600)
	writePIN(t, dir, "loose", "s3cret", 0o644)
	writePIN(t, dir, "empty", "\n", 0o600)

	if got, err := recipientkey.FilePIN(good)(); err != nil || got != "s3cret" {
		t.Errorf("FilePIN = %q, %v; want s3cret (one line break dropped)", got, err)
	}
	for _, name := range []string{"loose", "empty"} {
		if _, err := recipientkey.FilePIN(filepath.Join(dir, name))(); err == nil {
			t.Errorf("FilePIN(%s) was accepted", name)
		}
	}

	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := recipientkey.PIN("", "")(); !errors.Is(err, recipientkey.ErrNoCredential) {
		t.Errorf("no $CREDENTIALS_DIRECTORY: got %v, want ErrNoCredential", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "relative/dir")
	if _, err := recipientkey.PIN("", "")(); err == nil {
		t.Error("a relative $CREDENTIALS_DIRECTORY was accepted")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if got, err := recipientkey.PIN("", "")(); err != nil || got != "s3cret" {
		t.Errorf("the default credential = %q, %v", got, err)
	}
	if _, err := recipientkey.CredentialPIN("absent")(); !errors.Is(err, recipientkey.ErrNoCredential) {
		t.Errorf("an absent credential: got %v, want ErrNoCredential", err)
	}
	if _, err := recipientkey.CredentialPIN("../escape")(); err == nil {
		t.Error("a credential name with a slash was accepted")
	}
}
