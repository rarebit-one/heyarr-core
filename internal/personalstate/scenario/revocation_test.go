package scenario_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// TestRevocationIsForwardOnly is #321's forward-only boundary, asserted end to
// end through a real peer store rather than left as prose. After device D is
// revoked from space S and the space is re-keyed (a pure re-key, ADR-0103: the
// next key epoch, the previous key sealed under the new one, nothing
// re-encrypted, snapshotted or compacted), TWO things must hold at once, and
// the honesty of the design is that BOTH are true:
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
// change from the copy it kept and its in-memory key, AFTER the peer has deleted
// D's wrapped key — so D can no longer open the space, and the decryption can
// only be coming from material D already held.
//
// SABOTAGE (the two the issue names):
//  1. re-wrap the NEW key for the revoked device too → the "D cannot read
//     post-rotation content" assertion fires: D's decode of the post-rotation
//     change would stop erroring, and the peer would still hold a wrapped key for
//     D (both asserted against below).
//  2. leave FUTURE content under the OLD key → the "new content is under the new
//     key" assertion fires: the post-rotation change would not decode under the
//     newest key alone (asserted below), and D — still holding the old key —
//     would decode it, tripping the forward-secrecy check.
func TestRevocationIsForwardOnly(t *testing.T) {
	t.Parallel()
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
		if _, err := peer.PutWrappedKey(ctx, sp.ID, w.Recipient, w.Wrapped, 0); err != nil {
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

	// ROTATION over S, revoking D (mirrors `space rotate` / `device revoke`): A
	// mints the epoch-1 key wrapped for A and R ONLY and seals the epoch-0 key
	// under it; the peer stores the history row, the new wraps, and drops every
	// epoch-0 wrap — D's included — in one compare-and-swap.
	rot, err := mgrA.Rotate(sp.ID, []client.Recipient{aRecip, rRecip})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	wraps := make([]store.RecipientWrap, 0, len(rot.Wrapped))
	for _, w := range rot.Wrapped {
		wraps = append(wraps, store.RecipientWrap{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	epoch, err := peer.RotateKey(ctx, sp.ID, 0, rot.SealedPrev, wraps, []string{dRecip.ID}, nil)
	if err != nil {
		t.Fatalf("rotating the key on the peer: %v", err)
	}
	if epoch != 1 || rot.Epoch != 1 {
		t.Fatalf("rotation landed at epoch %d (client %d), want 1", epoch, rot.Epoch)
	}

	// A writes a POST-rotation change under the NEW key. Nothing was compacted,
	// so the peer holds both changes; the post-rotation one is the newcomer.
	putItem(t, ctx, peer, mgrA, sp.ID, "post-rotation-track")
	all, err := peer.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var postChanges []protocol.EncryptedChange
	for _, ch := range all {
		if ch.ChangeID != held[0].ChangeID {
			postChanges = append(postChanges, ch)
		}
	}
	if len(all) != 2 || len(postChanges) != 1 {
		t.Fatalf("the peer holds %d changes (%d new), want the pre-rotation change kept and one new one — a re-key compacts nothing", len(all), len(postChanges))
	}

	// FORWARD-ONLY, THE POSITIVE HALF. D's wrapped key is gone, so D can no
	// longer open the space from the peer — yet it still decrypts the
	// pre-rotation change from the copy it already held and its in-memory key.
	// Real decryption of pre-rotation content, after the revocation.
	keys, err := peer.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Recipient == dRecip.ID {
			t.Fatal("the peer still holds a wrapped key for the revoked device — a successful read below would not prove D used HELD material")
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

	// THE REMAINING DEVICE opens the re-keyed space as any device does: it
	// unwraps the epoch-1 key and unrolls the history row back to epoch 0, so it
	// reads the whole log — the pre-rotation change included — with nothing
	// re-encrypted.
	rows, err := peer.KeyHistory(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]client.HistoryEntry, 0, len(rows))
	for _, r := range rows {
		history = append(history, client.HistoryEntry{Epoch: r.Epoch, SealedPrev: r.SealedPrev})
	}
	mgrR := client.New()
	if err := mgrR.OpenWithHistory(sp.ID, wrappedFor(t, ctx, peer, sp.ID, rRecip.ID), 1, history, client.NewKeyUnwrapper(rKey)); err != nil {
		t.Fatalf("the remaining device could not open the re-keyed space: %v", err)
	}
	after := readState(t, ctx, peer, mgrR, sp.ID)
	if !contains(after, "post-rotation-track") {
		t.Fatalf("the remaining device could not read post-rotation content: %v", after)
	}
	if !contains(after, "pre-rotation-track") {
		t.Fatalf("the remaining device lost the pre-rotation state — the key history did not reach it: %v", after)
	}

	// The "new content is under the new key" assertion (sabotage 2): the
	// post-rotation change decodes under the epoch-1 key ALONE, with no history
	// to fall back on.
	newest, ok := mgrR.SpaceKey(sp.ID)
	if !ok {
		t.Fatal("the remaining device holds no current key")
	}
	newestOnly := client.New()
	// Loaded as a lone key (epoch 0, no history): a ring of exactly one key.
	if err := newestOnly.Load(sp.ID, newest, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := statesync.DecodeAll(newestOnly, postChanges); err != nil {
		t.Fatalf("the post-rotation change does not decode under the new key — is the future under the new key? %v", err)
	}

	// SABOTAGE 1 guard: the peer holds a wrapped key for the two remaining
	// devices only, both at the new epoch — none for the revoked one.
	keys, err = peer.WrappedKeysFor(ctx, sp.ID)
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
		if k.Epoch != 1 {
			t.Fatalf("the peer holds a copy for %s at epoch %d, want 1", k.Recipient, k.Epoch)
		}
	}

	// The server never saw plaintext: no stored change, and no history row,
	// carries any item in the clear.
	for _, ch := range all {
		for _, item := range []string{"pre-rotation-track", "post-rotation-track"} {
			if bytes.Contains(ch.Ciphertext, []byte(item)) {
				t.Fatalf("peer's stored change %s contains the plaintext %q — the server can read it", ch.ChangeID, item)
			}
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
