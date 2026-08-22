package identity_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// fakePeers is the database half. It is a struct rather than a real catalog
// because the states worth testing here — a public key recorded with no private
// key, a key recorded twice — are states a real catalog will not let a test
// reach without corrupting it first.
type fakePeers struct {
	id        string
	pub       []byte
	algo      string
	recorded  int
	recordErr error
}

func (f *fakePeers) SelfIdentity(context.Context) (string, []byte, error) {
	return f.id, f.pub, nil
}

func (f *fakePeers) RecordSelfPublicKey(_ context.Context, algo string, pub []byte) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded++
	f.algo = algo
	f.pub = append([]byte(nil), pub...)
	return nil
}

func newCAS(t *testing.T) *cas.FS {
	t.Helper()
	store, err := cas.OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

const peerA = "01990000-0000-7000-8000-00000000000a"

func TestEnsureGeneratesAndBindsBothPlaces(t *testing.T) {
	dir := t.TempDir()
	peers := &fakePeers{id: peerA}
	store := newCAS(t)

	got, err := identity.Ensure(context.Background(), identity.Options{
		DataDir: dir, Peers: peers, CAS: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerID != peerA {
		t.Errorf("peer id = %q, want %q", got.PeerID, peerA)
	}
	if len(got.PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(got.PublicKey), ed25519.PublicKeySize)
	}
	if peers.algo != "ed25519" {
		t.Errorf("algorithm recorded as %q", peers.algo)
	}
	markerID, err := store.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	if markerID != peerA {
		t.Errorf("the CAS marker says %q, want %q", markerID, peerA)
	}

	// The public key must be derivable from what is on disk — a public key
	// recorded that the private key does not produce is the mismatch this
	// package refuses on the next start.
	raw, err := os.ReadFile(identity.KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	seedHex := strings.TrimPrefix(strings.TrimSpace(string(raw)), "ed25519-seed:")
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		t.Fatal(err)
	}
	derived := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !derived.Equal(got.PublicKey) {
		t.Error("the key on disk does not produce the public key that was recorded")
	}

	// Second call: nothing regenerated, nothing re-recorded.
	again, err := identity.Ensure(context.Background(), identity.Options{
		DataDir: dir, Peers: peers, CAS: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.PublicKey.Equal(got.PublicKey) {
		t.Error("the public key changed on the second Ensure")
	}
	if peers.recorded != 1 {
		t.Errorf("the public key was recorded %d times, want once", peers.recorded)
	}
}

func TestEnsureRefusesAMarkerNamingAnotherPeer(t *testing.T) {
	const peerB = "01990000-0000-7000-8000-00000000000b"
	dir := t.TempDir()
	store := newCAS(t)
	if err := store.BindPeer(peerB); err != nil {
		t.Fatal(err)
	}

	_, err := identity.Ensure(context.Background(), identity.Options{
		DataDir: dir, Peers: &fakePeers{id: peerA}, CAS: store,
	})
	if !errors.Is(err, identity.ErrIdentityConflict) {
		t.Fatalf("error = %v, want an identity conflict", err)
	}
	for _, want := range []string{peerA, peerB, store.MarkerPath()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// And nothing was written: a refusal that generates a key first leaves the
	// operator with a second identity to reason about.
	if _, statErr := os.Stat(identity.KeyPath(dir)); !os.IsNotExist(statErr) {
		t.Errorf("a private key was written despite the refusal: %v", statErr)
	}
}

func TestEnsureAdoptsAKeyWhoseDatabaseRowLostItsPublicKey(t *testing.T) {
	// The interrupted-first-start case: the key file was written and the row
	// was never updated. Regenerating would change the identity for no reason.
	dir := t.TempDir()
	peers := &fakePeers{id: peerA}
	store := newCAS(t)

	first, err := identity.Ensure(context.Background(), identity.Options{DataDir: dir, Peers: peers, CAS: store})
	if err != nil {
		t.Fatal(err)
	}
	peers.pub = nil // as if the row had never been updated

	second, err := identity.Ensure(context.Background(), identity.Options{DataDir: dir, Peers: peers, CAS: store})
	if err != nil {
		t.Fatal(err)
	}
	if !second.PublicKey.Equal(first.PublicKey) {
		t.Error("a database row with no public key caused a new identity to be generated")
	}
}

func TestEnsureRefusesAWorldReadablePrivateKey(t *testing.T) {
	dir := t.TempDir()
	peers := &fakePeers{id: peerA}
	store := newCAS(t)
	if _, err := identity.Ensure(context.Background(), identity.Options{DataDir: dir, Peers: peers, CAS: store}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(identity.KeyPath(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(identity.KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("the chmod did not apply: %#o", info.Mode().Perm())
	}

	_, err = identity.Ensure(context.Background(), identity.Options{DataDir: dir, Peers: peers, CAS: store})
	if !errors.Is(err, identity.ErrKeyPermissions) {
		t.Fatalf("error = %v, want a permissions refusal", err)
	}
}

func TestEnsureRefusesAKeyFileThatIsNotOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(identity.KeyPath(dir), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := identity.Ensure(context.Background(), identity.Options{
		DataDir: dir, Peers: &fakePeers{id: peerA}, CAS: newCAS(t),
	})
	if err == nil {
		t.Fatal("a file that is not a key was accepted")
	}
	if !strings.Contains(err.Error(), "not a heyarr peer key") {
		t.Errorf("error = %v, want it to say what the file should have been", err)
	}
}

func TestFormatPublicKeyIsAlgorithmPrefixedHex(t *testing.T) {
	pub := make([]byte, ed25519.PublicKeySize)
	for i := range pub {
		pub[i] = byte(i)
	}
	got := identity.FormatPublicKey(pub)
	if !strings.HasPrefix(got, "ed25519:") {
		t.Errorf("FormatPublicKey = %q, want an ed25519: prefix", got)
	}
	if got != "ed25519:"+hex.EncodeToString(pub) {
		t.Errorf("FormatPublicKey = %q", got)
	}
	if identity.FormatPublicKey(nil) != "" {
		t.Error("an absent key rendered as something other than the empty string")
	}
}
