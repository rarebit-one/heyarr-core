package cli

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"slices"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
)

// addSecondRecipient wraps the space's CURRENT key, at its current epoch, for a
// fresh X25519 key too (ADR-0103), so a rotation has someone to revoke while
// this device remains. It returns the recipient id to revoke.
func (h *psHarness) addSecondRecipient(spaceID string) string {
	h.t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	id := encryption.FormatPublicKey(priv.PublicKey().Bytes())
	h.addRecipient(spaceID, id)
	return id
}

// addRecipient wraps the space's CURRENT key, at its current epoch, for the
// recipient id — the recipient-add path of ADR-0103.
func (h *psHarness) addRecipient(spaceID, id string) {
	h.t.Helper()
	recip, err := psclient.ParseRecipient(id)
	if err != nil {
		h.t.Fatal(err)
	}
	mgr, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	key, _ := mgr.SpaceKey(spaceID)
	epoch, _ := mgr.Epoch(spaceID)
	wrapped, err := encryption.Seal(key, recip.Key)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.RewrapKeys(h.ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: recip.ID, Wrapped: wrapped, Epoch: epoch}}); err != nil {
		h.t.Fatal(err)
	}
}

// custody is the harness device's custody backend.
func (h *psHarness) custody() psclient.Custody {
	h.t.Helper()
	cust, err := custody.Select(custody.Options{DeviceDir: h.deviceDir})
	if err != nil {
		h.t.Fatal(err)
	}
	return cust
}

// rekey is the rotation as `space rotate` and `device revoke` run it
// (rotateSpace): open the space with its key history, then re-key it
// (ADR-0103).
func (h *psHarness) rekey(spaceID string, revoke ...string) (spaceRotateView, error) {
	h.t.Helper()
	return rotateSpace(h.ctx, h.client, h.custody(), spaceID, revoke)
}

// assertRekeyedAway checks the controller's side of a rotation that revoked
// one recipient: the space is at epoch 1, its one history row is stored, and
// only this device's copy remains — at the new epoch, the revoked copy gone.
func (h *psHarness) assertRekeyedAway(spaceID, revoked string) {
	h.t.Helper()
	keys, err := h.client.SpaceKeys(h.ctx, spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	if keys.KeyEpoch != 1 {
		h.t.Errorf("key_epoch = %d, want 1", keys.KeyEpoch)
	}
	if wrappedFor(keys.WrappedKeys, revoked) || len(keys.WrappedKeys) != 1 || keys.WrappedKeys[0].Epoch != 1 {
		h.t.Errorf("wraps after rotation = %+v, want only this device's, at epoch 1", keys.WrappedKeys)
	}
	hist, err := h.client.KeyHistory(h.ctx, spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Epoch != 1 {
		h.t.Errorf("key history = %+v, want the one row for epoch 1", hist)
	}
}

// assertHistoryIsLoadBearing proves a fresh read below reaches the
// pre-rotation content THROUGH the key history: a client holding only the
// newest key — no history unrolled — cannot decrypt a single change the space
// held before the rotation. Without this, a rotation that quietly re-encrypted
// (or left content under the new key) would pass the readability checks too.
func (h *psHarness) assertHistoryIsLoadBearing(spaceID string, before []string) {
	h.t.Helper()
	mgr, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	newest, ok := mgr.SpaceKey(spaceID)
	if !ok {
		h.t.Fatal("no current key after rotation")
	}
	epoch, _ := mgr.Epoch(spaceID)
	newestOnly := psclient.New()
	if err := newestOnly.Load(spaceID, newest, 0, nil); err != nil {
		h.t.Fatal(err)
	}
	changes, err := h.client.Changes(h.ctx, spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	seen := 0
	for _, ec := range changes {
		if !slices.Contains(before, ec.ChangeID) {
			continue
		}
		seen++
		if _, err := newestOnly.Decrypt(spaceID, ec.Ciphertext); err == nil {
			h.t.Errorf("pre-rotation change %s decrypts under the epoch-%d key alone — the history is not what reads it", ec.ChangeID, epoch)
		}
	}
	if seen != len(before) || seen == 0 {
		h.t.Errorf("the controller holds %d of the %d pre-rotation changes — a pure re-key compacts nothing", seen, len(before))
	}
	if _, ok, err := h.client.Snapshot(h.ctx, spaceID); err != nil || ok {
		h.t.Errorf("a rotation pushed a snapshot (%v, %v) — it must be a pure re-key", ok, err)
	}
}

// changeIDs lists the change ids the controller holds for a space.
func (h *psHarness) changeIDs(spaceID string) []string {
	h.t.Helper()
	changes, err := h.client.Changes(h.ctx, spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	ids := make([]string, 0, len(changes))
	for _, ec := range changes {
		ids = append(ids, ec.ChangeID)
	}
	return ids
}

// TestSpaceRotateRekeysEverySpaceKind (#698, ADR-0103): `space rotate` — the
// real rotateSpace path — rotates every kind of space the same way, as a pure
// re-key. For each kind: the epoch advances, the revoked recipient's copy is
// gone, nothing is compacted or snapshotted, and a FRESH open of the space reads
// all of its pre-rotation content through the key history, via the same reader
// a device serves it with.
func TestSpaceRotateRekeysEverySpaceKind(t *testing.T) {
	cases := []struct {
		name  string
		seed  func(h *psHarness, spaceID string)
		check func(h *psHarness, spaceID string)
	}{
		{
			name: "playlist",
			seed: func(h *psHarness, id string) {
				s := crdt.New()
				pushChanges(h, id, []crdt.Change{s.Add("tr:one"), s.Add("tr:two")})
			},
			check: func(h *psHarness, id string) {
				got, err := h.reader().Playlist(id)
				if err != nil {
					h.t.Fatal(err)
				}
				if !slices.Equal(got, []string{"tr:one", "tr:two"}) {
					h.t.Errorf("playlist after rotation = %v, want both items", got)
				}
			},
		},
		{
			name: "vault drive",
			seed: func(h *psHarness, id string) {
				h.vaultPushFile(id, "docs/policy.txt", bytes.Repeat([]byte("sealed before the rotation\n"), 9000))
			},
			check: func(h *psHarness, id string) {
				want := bytes.Repeat([]byte("sealed before the rotation\n"), 9000)
				if got := h.vaultPull(id, "docs/policy.txt"); !bytes.Equal(got, want) {
					h.t.Errorf("the pre-rotation file pulled back as %d bytes, want %d identical", len(got), len(want))
				}
				if got := h.vaultLs(id); !slices.Equal(got, []string{"docs/policy.txt"}) {
					h.t.Errorf("vault ls after rotation = %v, want the file", got)
				}
			},
		},
		{
			name: "starred",
			seed: func(h *psHarness, id string) {
				s := crdt.NewStarSet()
				pushChanges(h, id, []crdt.StarChange{s.Star("tr:one"), s.Star("tr:two")})
			},
			check: func(h *psHarness, id string) {
				got, err := h.reader().Starred(id)
				if err != nil {
					h.t.Fatal(err)
				}
				slices.Sort(got)
				if !slices.Equal(got, []string{"tr:one", "tr:two"}) {
					h.t.Errorf("starred after rotation = %v, want both items", got)
				}
			},
		},
		{
			name: "play history",
			seed: func(h *psHarness, id string) {
				l := crdt.NewPlayLog()
				pushChanges(h, id, []crdt.PlayChange{l.Record("tr:one"), l.Record("tr:one"), l.Record("tr:two")})
			},
			check: func(h *psHarness, id string) {
				got, err := h.reader().History(id)
				if err != nil {
					h.t.Fatal(err)
				}
				if got.NowPlaying != "tr:two" || len(got.Frequent) != 2 || got.Frequent[0].ID != "tr:one" || got.Frequent[0].Count != 2 {
					h.t.Errorf("play history after rotation = %+v, want tr:one twice then tr:two now playing", got)
				}
			},
		},
		{
			name: "reading position",
			seed: func(h *psHarness, id string) {
				r := crdt.NewReadingPositions()
				pushChanges(h, id, []crdt.PositionChange{r.Set("pub:one", "page 3"), r.Set("pub:one", "page 7")})
			},
			check: func(h *psHarness, id string) {
				got, err := h.reader().ReadingPositions(id)
				if err != nil {
					h.t.Fatal(err)
				}
				if len(got) != 1 || got[0].PubID != "pub:one" || got[0].Position != "page 7" {
					h.t.Errorf("reading positions after rotation = %+v, want pub:one at page 7", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPSHarness(t, auth.ScopeAdmin)
			spaceID := h.createSpace()
			tc.seed(h, spaceID)
			other := h.addSecondRecipient(spaceID)
			before := h.changeIDs(spaceID)

			view, err := h.rekey(spaceID, other)
			if err != nil {
				t.Fatalf("rotate: %v", err)
			}
			if view.KeyEpoch != 1 || len(view.Revoked) != 1 || view.Revoked[0] != other || len(view.Remaining) != 1 {
				t.Fatalf("rotate view = %+v", view)
			}
			h.assertRekeyedAway(spaceID, other)
			h.assertHistoryIsLoadBearing(spaceID, before)
			tc.check(h, spaceID)
		})
	}
	t.Run("empty", func(t *testing.T) {
		h := newPSHarness(t, auth.ScopeAdmin)
		spaceID := h.createSpace()
		other := h.addSecondRecipient(spaceID)
		if _, err := h.rekey(spaceID, other); err != nil {
			t.Fatal(err)
		}
		h.assertRekeyedAway(spaceID, other)
	})
}

// TestDeviceRevokeSweepRekeysEverySpaceKind: `device revoke` re-keys EVERY
// space the revoked device could read through the same rotation, a vault space
// as readily as a playlist one, and skips nothing. Both stay readable here,
// through the key history.
func TestDeviceRevokeSweepRekeysEverySpaceKind(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	playlist := h.createSpace()
	s := crdt.New()
	pushChanges(h, playlist, []crdt.Change{s.Add("tr:one")})
	other := h.addSecondRecipient(playlist)

	vault := h.createSpace()
	file := []byte("a vault file sealed before the revocation")
	h.vaultPushFile(vault, "docs/a.txt", file)
	h.addRecipient(vault, other) // the same revoked device holds a copy here too

	rotated, skipped, err := rotateAwayFrom(h.ctx, h.client, h.custody(), other)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want nothing — every space kind rotates", skipped)
	}
	got := map[string]int{}
	for _, r := range rotated {
		got[r.SpaceID] = r.KeyEpoch
	}
	if len(rotated) != 2 || got[playlist] != 1 || got[vault] != 1 {
		t.Fatalf("rotated = %+v, want the playlist and the vault space, each at epoch 1", rotated)
	}
	h.assertRekeyedAway(playlist, other)
	h.assertRekeyedAway(vault, other)

	if items, err := h.reader().Playlist(playlist); err != nil || !slices.Equal(items, []string{"tr:one"}) {
		t.Fatalf("playlist after the sweep = %v (%v), want the item", items, err)
	}
	if pulled := h.vaultPull(vault, "docs/a.txt"); !bytes.Equal(pulled, file) {
		t.Fatalf("the vault file after the sweep pulled back as %q", pulled)
	}
}
