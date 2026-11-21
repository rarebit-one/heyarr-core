package statesync_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// TestTamperedFrontierIsRefusedAndTheBaseStillSeeds is #681 end to end. After a
// compaction, a principal with only a `write` token relabels the base: same
// ciphertext, a frontier naming a forged tail change it pushed, the public id
// recomputed. The peer cannot tell (it has no key), and the forged frontier does
// cover the base causally, so the peer offers it as the cold-start snapshot. The
// device refuses it on the envelope, falls back to the pinned base, and reaches
// the full state from the base plus the tail.
//
// SABOTAGE: have OpenSnapshotPlaintext skip the frontier comparison, and the
// relabelled snapshot is accepted as a seed whose frontier the state never
// folded: the refusal assertion fires.
func TestTamperedFrontierIsRefusedAndTheBaseStillSeeds(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	dev, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recip := client.Recipient{ID: encryption.FormatPublicKey(dev.PublicKey().Bytes()), Key: dev.PublicKey()}
	m := client.New()
	sp, wrapped, err := m.Create(spaces.KindPersonal, time.Now().UTC(), []client.Recipient{recip})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSpace(ctx, sp.ID, sp.Kind); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"trusted-item-one", "trusted-item-two"} {
		put(t, ctx, s, m, sp.ID, item)
	}
	full := readState(t, ctx, s, m, sp.ID)
	heads, _ := s.HeadsFor(ctx, sp.ID)
	changes, _ := s.ChangesFor(ctx, sp.ID)
	decodedAll, err := statesync.DecodeAll(m, changes)
	if err != nil {
		t.Fatal(err)
	}
	folded := crdt.New()
	folded.Apply(decodedAll...)
	base, err := statesync.EncodeSnapshot(m, sp.ID, heads, folded)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	if res, err := s.CompactChanges(ctx, sp.ID, base.SnapshotID, nil); err != nil || res.Dropped != 2 {
		t.Fatalf("compaction = %+v err=%v", res, err)
	}
	// The tail is parented on the base's frontier, as currentHeads does once
	// the log is compacted to nothing.
	tailChange, err := statesync.Encode(m, sp.ID, base.Frontier, crdt.New().Add("trusted-item-tail"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutChange(ctx, tailChange); err != nil {
		t.Fatal(err)
	}

	// The keyless forgery: a junk change parented on the real tail, and the
	// base's ciphertext relabelled to it.
	tail, _ := s.HeadsFor(ctx, sp.ID)
	junk, _ := protocol.NewChange(sp.ID, tail, []byte("not-ciphertext-of-anything"))
	if err := s.PutChange(ctx, junk); err != nil {
		t.Fatal(err)
	}
	forged, err := protocol.NewSnapshot(sp.ID, []string{junk.ChangeID}, base.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, forged); err != nil {
		t.Fatal(err)
	}

	fresh := client.New()
	if err := fresh.Open(sp.ID, wrapped[0].Wrapped, client.NewKeyUnwrapper(dev)); err != nil {
		t.Fatal(err)
	}
	offered, ok, err := s.ColdStartSnapshot(ctx, sp.ID)
	if err != nil || !ok || offered.SnapshotID != forged.SnapshotID {
		t.Fatalf("precondition: the keyless peer offers the forgery (it covers the base); got %s ok=%v err=%v", offered.SnapshotID, ok, err)
	}
	if _, err := statesync.DecodeSnapshot(fresh, offered); !errors.Is(err, protocol.ErrSnapshotBindingMismatch) {
		t.Fatalf("decoding the relabelled snapshot: err = %v, want ErrSnapshotBindingMismatch", err)
	}

	// The fallback: the pinned base verifies, and base + tail is the full state.
	pinned, ok, err := s.Base(ctx, sp.ID)
	if err != nil || !ok {
		t.Fatalf("Base: ok=%v err=%v", ok, err)
	}
	raw, authenticated, err := statesync.OpenSnapshot(fresh, pinned)
	if err != nil || !authenticated {
		t.Fatalf("the base must verify: authenticated=%v err=%v", authenticated, err)
	}
	st, err := crdt.FromSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	rest, _ := s.ChangesFor(ctx, sp.ID)
	var tailOnly []protocol.EncryptedChange
	for _, c := range protocol.Missing(rest, pinned.Frontier) {
		if c.ChangeID != junk.ChangeID { // a reader skips what it cannot decrypt
			tailOnly = append(tailOnly, c)
		}
	}
	decoded, err := statesync.DecodeAll(fresh, tailOnly)
	if err != nil {
		t.Fatal(err)
	}
	st.Apply(decoded...)
	want := append(slices.Clone(full), "trusted-item-tail")
	slices.Sort(want)
	got := st.IDs()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("base + tail = %v, want %v", got, want)
	}
}
