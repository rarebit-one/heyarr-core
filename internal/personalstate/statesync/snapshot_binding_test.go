package statesync_test

import (
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// openedSpace creates a space on a fresh manager, returning it with the space id.
func openedSpace(t *testing.T) (*client.Manager, string) {
	t.Helper()
	dev, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recip := client.Recipient{ID: encryption.FormatPublicKey(dev.PublicKey().Bytes()), Key: dev.PublicKey()}
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, time.Now().UTC(), []client.Recipient{recip})
	if err != nil {
		t.Fatal(err)
	}
	return m, sp.ID
}

// TestRelabelledSnapshotIsRefused is #681: a principal with a `write` token but no
// space key can fetch a valid snapshot, swap its frontier, recompute the public id
// and push it back. The id then validates and the ciphertext still decrypts, so
// before the envelope a device would have accepted the forged causal point. Every
// relabelling below must now be refused by the decoder, with the binding error.
func TestRelabelledSnapshotIsRefused(t *testing.T) {
	m, spaceID := openedSpace(t)
	st := crdt.New()
	st.Apply(st.Add("item-1"))
	sealedAt := []string{"blake3:aa", "blake3:bb"}
	snap, err := statesync.EncodeSnapshot(m, spaceID, sealedAt, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statesync.DecodeSnapshot(m, snap); err != nil {
		t.Fatalf("the genuine snapshot must decode: %v", err)
	}

	for _, tc := range []struct {
		name     string
		frontier []string
	}{
		{"advanced to newer heads", []string{"blake3:cc"}},
		{"one head dropped", []string{"blake3:aa"}},
		{"one head added", []string{"blake3:aa", "blake3:bb", "blake3:cc"}},
		{"frontier emptied", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Everything the attacker does needs no key: reuse the ciphertext,
			// re-mint the id over the new frontier.
			forged, err := protocol.NewSnapshot(spaceID, tc.frontier, snap.Ciphertext)
			if err != nil {
				t.Fatal(err)
			}
			if err := forged.Validate(); err != nil {
				t.Fatalf("precondition: the forged id must validate, or the test proves nothing: %v", err)
			}
			if _, err := statesync.DecodeSnapshot(m, forged); err == nil {
				t.Fatal("a snapshot relabelled to a different frontier decoded; its frontier is not authenticated")
			} else if !errors.Is(err, protocol.ErrSnapshotBindingMismatch) {
				t.Fatalf("refused, but not for the binding: %v", err)
			}
		})
	}

	// The same frontier in another order is the same causal point, not a relabel.
	reordered, err := protocol.NewSnapshot(spaceID, []string{"blake3:bb", "blake3:aa", "blake3:aa"}, snap.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statesync.DecodeSnapshot(m, reordered); err != nil {
		t.Fatalf("a non-canonical spelling of the sealed frontier must still decode: %v", err)
	}
}

// TestLegacySnapshotDecodesUnauthenticated pins the compatibility path: a snapshot
// sealed before the envelope (the bare state encrypted, as `space rotate` wrote
// them) still decodes, because after a rotation it can be the only copy of the
// state — but OpenSnapshot reports its frontier unauthenticated, and a snapshot
// written now reports authenticated.
func TestLegacySnapshotDecodesUnauthenticated(t *testing.T) {
	m, spaceID := openedSpace(t)
	st := crdt.New()
	st.Apply(st.Add("item-1"))
	raw, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := m.Encrypt(spaceID, raw)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := protocol.NewSnapshot(spaceID, []string{"blake3:aa"}, ct)
	if err != nil {
		t.Fatal(err)
	}
	got, err := statesync.DecodeSnapshot(m, legacy)
	if err != nil {
		t.Fatalf("a legacy snapshot must still decode: %v", err)
	}
	if want := st.IDs(); len(got.IDs()) != len(want) || got.IDs()[0] != want[0] {
		t.Fatalf("legacy state lost: got %v want %v", got.IDs(), want)
	}
	if _, authed, err := statesync.OpenSnapshot(m, legacy); err != nil || authed {
		t.Fatalf("legacy snapshot: authenticated=%v err=%v; want false, nil", authed, err)
	}

	current, err := statesync.EncodeSnapshot(m, spaceID, []string{"blake3:aa"}, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, authed, err := statesync.OpenSnapshot(m, current); err != nil || !authed {
		t.Fatalf("enveloped snapshot: authenticated=%v err=%v; want true, nil", authed, err)
	}
}

// TestChangeCiphertextIsNotALegacySnapshot is the review finding on #683: a
// keyless `write` holder copies a CHANGE's ciphertext into a freshly minted
// snapshot. It has no envelope, so it takes the legacy path, and the permissive
// playlist decoder reads the change JSON as an empty state. A reader starting
// from it would drop everything compaction removed from the log. The legacy path
// must refuse any plaintext that is not a canonical snapshot.
func TestChangeCiphertextIsNotALegacySnapshot(t *testing.T) {
	m, spaceID := openedSpace(t)
	st := crdt.New()
	ch, err := statesync.Encode(m, spaceID, nil, st.Add("item-1"))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := protocol.NewSnapshot(spaceID, []string{ch.ChangeID}, ch.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statesync.DecodeSnapshot(m, forged); !errors.Is(err, statesync.ErrLegacySnapshotNotCanonical) {
		t.Fatalf("a change's ciphertext presented as a snapshot: err=%v, want ErrLegacySnapshotNotCanonical", err)
	}
}
