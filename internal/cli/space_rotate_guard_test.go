package cli

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/custody"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
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

// rotate is the guarded rotation, as `space rotate` and `device revoke` run it.
func (h *psHarness) rotate(spaceID, revoke string) (spaceRotateView, error) {
	h.t.Helper()
	return rotateSpace(h.ctx, h.client, h.custody(), spaceID, []string{revoke})
}

// rekey is the unguarded re-key (ADR-0103), which every space kind survives.
func (h *psHarness) rekey(spaceID string, revoke ...string) (spaceRotateView, error) {
	h.t.Helper()
	return rekeySpace(h.ctx, h.client, h.custody(), spaceID, revoke)
}

// A client that predates key epochs reads a rotated space with its newest key
// only, which strands every non-playlist space's earlier content (#698). Until
// every client unrolls the key history (ADR-0103), the guarded rotation must
// refuse those spaces outright, and leave them exactly as they were: the same
// changes, the same wrapped keys, the same epoch.
func TestSpaceRotateRefusesNonPlaylistSpaces(t *testing.T) {
	cases := []struct {
		name string
		seed func(h *psHarness, spaceID string)
	}{
		{"vault drive", func(h *psHarness, id string) {
			d := crdt.NewDrive()
			ch, err := d.Put("docs/policy.pdf", "blake3:"+strings.Repeat("ab", 32), 42, 1)
			if err != nil {
				h.t.Fatal(err)
			}
			pushChanges(h, id, []crdt.DriveChange{ch})
		}},
		{"starred", func(h *psHarness, id string) {
			pushChanges(h, id, []crdt.StarChange{crdt.NewStarSet().Star("tr:one")})
		}},
		{"play history", func(h *psHarness, id string) {
			pushChanges(h, id, []crdt.PlayChange{crdt.NewPlayLog().Record("tr:one")})
		}},
		{"reading position", func(h *psHarness, id string) {
			pushChanges(h, id, []crdt.PositionChange{crdt.NewReadingPositions().Set("pub:one", "page 3")})
		}},
		{"drive snapshot only", func(h *psHarness, id string) {
			d := crdt.NewDrive()
			ch, err := d.Put("docs/a.txt", "blake3:"+strings.Repeat("cd", 32), 1, 1)
			if err != nil {
				h.t.Fatal(err)
			}
			d.Apply(ch)
			snap, err := statesync.EncodeDriveSnapshot(h.mgr, id, nil, d)
			if err != nil {
				h.t.Fatal(err)
			}
			if _, err := h.client.PushSnapshot(h.ctx, snap); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPSHarness(t, auth.ScopeAdmin)
			spaceID := h.createSpace()
			tc.seed(h, spaceID)
			other := h.addSecondRecipient(spaceID)

			before, err := h.client.Changes(h.ctx, spaceID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.rotate(spaceID, other)
			if !errors.Is(err, errRotateNotPlaylist) {
				t.Fatalf("rotate err = %v, want errRotateNotPlaylist", err)
			}
			if !strings.Contains(err.Error(), "#698") {
				t.Errorf("refusal does not point at the issue: %v", err)
			}

			after, err := h.client.Changes(h.ctx, spaceID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Errorf("changes after refusal = %d, want %d (nothing compacted)", len(after), len(before))
			}
			keys, err := h.client.WrappedKeys(h.ctx, spaceID)
			if err != nil {
				t.Fatal(err)
			}
			if !wrappedFor(keys, other) || len(keys) != 2 {
				t.Errorf("wrapped keys after refusal = %d (other kept: %v), want both untouched", len(keys), wrappedFor(keys, other))
			}
			if sk, err := h.client.SpaceKeys(h.ctx, spaceID); err != nil || sk.KeyEpoch != 0 {
				t.Errorf("epoch after refusal = %d (%v), want 0", sk.KeyEpoch, err)
			}
		})
	}
}

// A playlist space, and an empty one, still rotate: the guard only stops the
// spaces rotation would destroy.
func TestSpaceRotateStillRotatesPlaylistAndEmptySpaces(t *testing.T) {
	t.Run("playlist", func(t *testing.T) {
		h := newPSHarness(t, auth.ScopeAdmin)
		spaceID := h.createSpace()
		s := crdt.New()
		pushChanges(h, spaceID, []crdt.Change{s.Add("tr:one"), s.Add("tr:two")})
		other := h.addSecondRecipient(spaceID)

		view, err := h.rotate(spaceID, other)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Revoked) != 1 || view.Revoked[0] != other || view.KeyEpoch != 1 || len(view.Remaining) != 1 {
			t.Errorf("rotate view = %+v", view)
		}
		// Until the guard lifts, a playlist is also carried forward for clients
		// without a keyring: a snapshot under the new key, the old log compacted.
		if view.SnapshotID == "" || view.Dropped != 2 {
			t.Errorf("playlist not carried forward: snapshot %q, dropped %d", view.SnapshotID, view.Dropped)
		}
		// A client with ONLY the new key (no history) still reads the playlist.
		cur, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
		if err != nil {
			t.Fatal(err)
		}
		newKey, ok := cur.SpaceKey(spaceID)
		if !ok {
			t.Fatal("no current key after rotation")
		}
		legacy := psclient.New()
		if err := legacy.Load(spaceID, newKey, 0, nil); err != nil {
			t.Fatal(err)
		}
		lst, _, _, err := materialise(h.ctx, h.client, legacy, spaceID)
		if err != nil {
			t.Fatalf("a keyring-less client cannot read the rotated playlist: %v", err)
		}
		if got := lst.IDs(); len(got) != 2 {
			t.Errorf("keyring-less playlist = %v, want both items", got)
		}
		// And a keyring client reads it too.
		mgr, err := openSpace(h.ctx, h.client, h.custody(), spaceID)
		if err != nil {
			t.Fatal(err)
		}
		st, _, _, err := materialise(h.ctx, h.client, mgr, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.IDs(); len(got) != 2 {
			t.Errorf("playlist after rotation = %v, want both items", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		h := newPSHarness(t, auth.ScopeAdmin)
		spaceID := h.createSpace()
		other := h.addSecondRecipient(spaceID)
		if _, err := h.rotate(spaceID, other); err != nil {
			t.Fatal(err)
		}
	})
}

// `device revoke` rotates through the guarded path, so until every client
// understands key epochs it still reports a non-playlist space as skipped (and
// leaves it untouched) while re-keying the playlist space beside it.
func TestDeviceRevokeSweepStillSkipsNonPlaylistSpaces(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	playlist := h.createSpace()
	s := crdt.New()
	pushChanges(h, playlist, []crdt.Change{s.Add("tr:one")})
	other := h.addSecondRecipient(playlist)

	vault := h.createSpace()
	d := crdt.NewDrive()
	ch, err := d.Put("docs/a.txt", "blake3:"+strings.Repeat("ef", 32), 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	pushChanges(h, vault, []crdt.DriveChange{ch})
	// The same revoked recipient on the vault space too.
	mgr, err := openSpace(h.ctx, h.client, h.custody(), vault)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := mgr.SpaceKey(vault)
	pub, err := encryption.ParsePublicKey(other)
	if err != nil {
		t.Fatal(err)
	}
	w, err := encryption.Seal(key, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.RewrapKeys(h.ctx, vault, []apiclient.WrappedKeyInput{{Recipient: other, Wrapped: w}}); err != nil {
		t.Fatal(err)
	}

	rotated, skipped, err := rotateAwayFrom(h.ctx, h.client, h.custody(), other)
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated) != 1 || rotated[0].SpaceID != playlist || rotated[0].KeyEpoch != 1 {
		t.Fatalf("rotated = %+v, want the playlist space at epoch 1", rotated)
	}
	if len(skipped) != 1 || skipped[0].SpaceID != vault || !strings.Contains(skipped[0].Reason, "#698") {
		t.Fatalf("skipped = %+v, want the vault space, pointing at #698", skipped)
	}
	keys, err := h.client.SpaceKeys(h.ctx, vault)
	if err != nil {
		t.Fatal(err)
	}
	if keys.KeyEpoch != 0 || !wrappedFor(keys.WrappedKeys, other) {
		t.Fatalf("the skipped vault space was touched: epoch %d, other kept %v", keys.KeyEpoch, wrappedFor(keys.WrappedKeys, other))
	}
}
