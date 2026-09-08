package scenario_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// TestRevocationIsForwardOnly is #321's forward-only boundary, asserted end to
// end through a real peer store rather than left as prose. After device D is
// revoked from space S and the space is re-keyed, TWO things must hold at once,
// and the honesty of the design is that BOTH are true:
//
//   - the POSITIVE half (the one ADR-0022 promises and this issue asks to state
//     in a test): D can STILL decrypt a pre-rotation change it already held —
//     revocation keeps what a device already decrypted, it is forward-looking and
//     not retroactive. A test that pretended otherwise would claim a protection
//     the mechanism cannot give.
//   - the NEGATIVE half: every FUTURE change is beyond D and only the remaining
//     device reads it — a rotation that left the future readable would be no
//     revocation at all.
//
// The positive half is exercised, not narrated: D decrypts the pre-rotation
// change from the copy it kept and its in-memory key, AFTER the peer has both
// deleted D's wrapped key and compacted the change away — so the decryption can
// only be coming from material D already held.
//
// SABOTAGE (the two the issue names):
//  1. re-wrap the NEW key for the revoked device too → the "D cannot read
//     post-rotation content" assertion fires: D's decode of the post-rotation
//     change would stop erroring, and the peer would still hold a wrapped key for
//     D (both asserted against below).
//  2. leave FUTURE content under the OLD key → the "new content is under the new
//     key" assertion fires: the remaining device, holding only the NEW key, would
//     fail to decode the post-rotation change (asserted below), and D — still
//     holding the old key — would decode it, tripping the forward-secrecy check.
func TestRevocationIsForwardOnly(t *testing.T) {
	ctx := context.Background()
	peer := newStore(t)

	// Three devices, each with its own X25519 encryption key: A creates and
	// remains, D will be revoked, R remains.
	_, aRecip := recipient(t)
	dKey, dRecip := recipient(t)
	rKey, rRecip := recipient(t)

	// A mints the space wrapped for A, D and R, and persists the opaque space and
	// the wrapped keys on the peer (as the create route would). A holds the key.
	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindFamily, injected, []client.Recipient{aRecip, dRecip, rRecip})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.PutSpace(ctx, sp.ID, sp.Kind); err != nil {
		t.Fatal(err)
	}
	for _, w := range wrapped {
		if _, err := peer.PutWrappedKey(ctx, sp.ID, w.Recipient, w.Wrapped); err != nil {
			t.Fatal(err)
		}
	}

	// D opens the space from its wrapped copy — it is a recipient at this point.
	mgrD := client.New()
	if err := mgrD.Open(sp.ID, wrappedFor(t, ctx, peer, sp.ID, dRecip.ID), client.NewKeyUnwrapper(dKey)); err != nil {
		t.Fatalf("device D could not open the space it was wrapped for: %v", err)
	}

	// A writes a PRE-rotation change; D reads it while still authorised. This is
	// the content D "already held".
	putItem(t, ctx, peer, mgrA, sp.ID, "pre-rotation-track")
	if got := readState(t, ctx, peer, mgrD, sp.ID); !contains(got, "pre-rotation-track") {
		t.Fatalf("D could not read the pre-rotation change while authorised: %v", got)
	}

	// What D keeps across the revocation: the pre-rotation change as ciphertext,
	// and its space key in memory. Nothing takes an in-memory key away.
	held, err := peer.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) == 0 {
		t.Fatal("the peer holds no pre-rotation change to keep — the setup proved nothing")
	}
	preHeads, err := peer.HeadsFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}

	// ROTATION over S, revoking D (mirrors `space rotate` / `device revoke`):
	// materialise the current state under the OLD key, mint a fresh key sealed for
	// A and R ONLY, delete D's stored copy, snapshot the state under the new key,
	// and compact the old change the snapshot subsumes.
	preState := crdt.New()
	preState.Apply(mustDecode(t, mgrA, held)...)
	newWrapped, err := mgrA.Rotate(sp.ID, []client.Recipient{aRecip, rRecip})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	for _, w := range newWrapped {
		if _, err := peer.PutWrappedKey(ctx, sp.ID, w.Recipient, w.Wrapped); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.DeleteWrappedKey(ctx, sp.ID, dRecip.ID); err != nil {
		t.Fatalf("deleting the revoked device's wrapped key: %v", err)
	}
	snap, err := statesync.EncodeSnapshot(mgrA, sp.ID, preHeads, preState)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.CompactChanges(ctx, sp.ID, preHeads); err != nil {
		t.Fatalf("compacting the pre-rotation log: %v", err)
	}

	// A writes a POST-rotation change under the NEW key. The peer now holds only
	// this change (the pre-rotation one was compacted) plus the new-key snapshot.
	putItem(t, ctx, peer, mgrA, sp.ID, "post-rotation-track")
	postChanges, err := peer.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}

	// FORWARD-ONLY, THE POSITIVE HALF. The peer can no longer serve the
	// pre-rotation change — D's wrapped key is gone AND the change was compacted
	// away — yet D still decrypts it from the copy it already held and its
	// in-memory key. Real decryption of pre-rotation content, after the revocation.
	for _, ch := range postChanges {
		if ch.ChangeID == held[0].ChangeID {
			t.Fatal("the pre-rotation change is still on the peer — compaction did not drop it, so a successful read below would not prove D used HELD material")
		}
	}
	kept := crdt.New()
	keptChanges, err := statesync.DecodeAll(mgrD, held)
	if err != nil {
		t.Fatalf("the revoked device could not decrypt a pre-rotation change it already held — revocation must be forward-looking, not retroactive: %v", err)
	}
	kept.Apply(keptChanges...)
	if got := kept.IDs(); !contains(got, "pre-rotation-track") {
		t.Fatalf("the revoked device lost the pre-rotation change it already held: %v", got)
	}

	// FORWARD SECRECY. D, holding only the old key, cannot decrypt the
	// post-rotation change. If it could, either the new key was re-wrapped for it
	// (sabotage 1) or the future content was left under the old key (sabotage 2).
	if _, err := statesync.DecodeAll(mgrD, postChanges); err == nil {
		t.Fatal("the revoked device read post-rotation content — forward secrecy is broken")
	}

	// THE REMAINING DEVICE reads the future content, under the NEW key, from the
	// snapshot plus the tail. This is the "new content is under the new key"
	// assertion: were the post-rotation change left under the OLD key, R (new key
	// only) would fail to decode it.
	mgrR := client.New()
	if err := mgrR.Open(sp.ID, wrappedFor(t, ctx, peer, sp.ID, rRecip.ID), client.NewKeyUnwrapper(rKey)); err != nil {
		t.Fatalf("the remaining device could not open the re-keyed space: %v", err)
	}
	base, err := statesync.DecodeSnapshot(mgrR, snap)
	if err != nil {
		t.Fatalf("the remaining device could not decode the new-key snapshot: %v", err)
	}
	base.Apply(mustDecode(t, mgrR, postChanges)...)
	after := base.IDs()
	if !contains(after, "post-rotation-track") {
		t.Fatalf("the remaining device could not read post-rotation content — is the future under the new key? %v", after)
	}
	if !contains(after, "pre-rotation-track") {
		t.Fatalf("the remaining device lost the pre-rotation state carried into the new-key snapshot: %v", after)
	}

	// SABOTAGE 1 guard: the peer holds a wrapped key for the two remaining
	// devices only — none for the revoked one.
	keys, err := peer.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("after revocation the peer holds %d wrapped keys, want 2 (A and R)", len(keys))
	}
	for _, k := range keys {
		if k.Recipient == dRecip.ID {
			t.Fatal("the peer still holds a wrapped key for the revoked device — the new key was re-wrapped for it")
		}
	}

	// The server never saw plaintext: neither the post-rotation change nor the
	// snapshot carries any item in the clear.
	for _, ch := range postChanges {
		if bytes.Contains(ch.Ciphertext, []byte("post-rotation-track")) {
			t.Fatalf("peer's stored post-rotation change %s contains the plaintext — the server can read it", ch.ChangeID)
		}
	}
	for _, item := range []string{"pre-rotation-track", "post-rotation-track"} {
		if bytes.Contains(snap.Ciphertext, []byte(item)) {
			t.Fatalf("the new-key snapshot at rest contains the plaintext %q — it is not opaque", item)
		}
	}
}

// recipient mints a device X25519 encryption key and its rendered recipient.
func recipient(t *testing.T) (*ecdh.PrivateKey, client.Recipient) {
	t.Helper()
	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key, client.Recipient{ID: encryption.FormatPublicKey(key.PublicKey().Bytes()), Key: key.PublicKey()}
}

// mustDecode decrypts changes under a manager's open key, failing the test on any
// error — used where a decode is setup rather than the assertion under test.
func mustDecode(t *testing.T, m *client.Manager, changes []protocol.EncryptedChange) []crdt.Change {
	t.Helper()
	decoded, err := statesync.DecodeAll(m, changes)
	if err != nil {
		t.Fatalf("decoding changes: %v", err)
	}
	return decoded
}
