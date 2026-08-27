package statesync_test

import (
	"bytes"
	"crypto/ecdh"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

var testNow = time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)

func device(t *testing.T) (*ecdh.PrivateKey, client.Recipient, client.Unwrapper) {
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

func wrappedFor(t *testing.T, ws []client.WrappedFor, id string) []byte {
	t.Helper()
	for _, w := range ws {
		if w.Recipient == id {
			return w.Wrapped
		}
	}
	t.Fatalf("no wrapped key for %s", id)
	return nil
}

// TestEncodeDecodeRoundTrip: a CRDT change encrypted and wrapped as an opaque
// change decodes back to the same change on a device holding the space key.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	state := crdt.New()
	ch := state.Add("song-1")

	ec, err := statesync.Encode(m, sp.ID, nil, ch)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := statesync.Decode(m, ec)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if back.ItemID != ch.ItemID || back.Tag != ch.Tag || back.Op != ch.Op {
		t.Fatalf("round trip changed the CRDT change: %+v vs %+v", back, ch)
	}
}

// TestTwoDevicesConvergeThroughEncryptedChanges is #324's core at the unit level:
// device A makes CRDT edits, encrypts them into opaque changes; device B — holding
// only its wrapped copy of the space key — decrypts them and merges, and the two
// devices' playlists converge. The peer between them never sees plaintext.
func TestTwoDevicesConvergeThroughEncryptedChanges(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	_, rb, ub := device(t)

	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindShared, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}

	// A edits its playlist and ships the changes as opaque encrypted changes.
	stateA := crdt.New()
	var wire []protocol.EncryptedChange
	for _, item := range []string{"alpha", "bravo", "charlie"} {
		ch := stateA.Add(item)
		ec, err := statesync.Encode(mgrA, sp.ID, nil, ch)
		if err != nil {
			t.Fatalf("Encode %s: %v", item, err)
		}
		wire = append(wire, ec)
	}

	// B holds only its wrapped copy; it opens the space and merges the changes.
	mgrB := client.New()
	if err := mgrB.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	decoded, err := statesync.DecodeAll(mgrB, wire)
	if err != nil {
		t.Fatalf("B.DecodeAll: %v", err)
	}
	stateB := crdt.New()
	stateB.Apply(decoded...)

	if a, b := stateA.IDs(), stateB.IDs(); !equalStrings(a, b) {
		t.Fatalf("devices did not converge: A=%v B=%v", a, b)
	}
	// And convergence is order-independent — B applying in reverse still converges.
	reverse := make([]crdt.Change, len(decoded))
	for i := range decoded {
		reverse[len(decoded)-1-i] = decoded[i]
	}
	stateB2 := crdt.New()
	stateB2.Apply(reverse...)
	if !equalStrings(stateB.IDs(), stateB2.IDs()) {
		t.Fatal("merge order changed the converged state")
	}
}

// TestNonRecipientCannotDecode: a device without the space key cannot decode an
// opaque change — the confidentiality the wrap protects.
func TestNonRecipientCannotDecode(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	mgrA := client.New()
	sp, _, err := mgrA.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	ec, err := statesync.Encode(mgrA, sp.ID, nil, crdt.New().Add("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// A stranger that never opened the space cannot decode.
	stranger := client.New()
	if _, err := statesync.Decode(stranger, ec); err == nil {
		t.Fatal("a device without the space key decoded a change")
	}
}

// TestDecodeRejectsForgedChange: a change whose id does not match its bytes is
// refused before decryption — a client trusts a claimed id no more than a peer.
func TestDecodeRejectsForgedChange(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	ec, err := statesync.Encode(m, sp.ID, nil, crdt.New().Add("x"))
	if err != nil {
		t.Fatal(err)
	}
	ec.Ciphertext = append([]byte("tamper"), ec.Ciphertext...) // id no longer matches
	if _, err := statesync.Decode(m, ec); err == nil {
		t.Fatal("a forged change decoded")
	}
}

// TestChangeIsOpaqueToThePeer: the wire change's ciphertext does not contain the
// CRDT change's plaintext (the item id) — a peer holding it learns nothing.
func TestChangeIsOpaqueToThePeer(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	ec, err := statesync.Encode(m, sp.ID, nil, crdt.New().Add("a-secret-song-title"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ec.Ciphertext, []byte("a-secret-song-title")) {
		t.Fatal("the encrypted change leaks the item id in the clear")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
