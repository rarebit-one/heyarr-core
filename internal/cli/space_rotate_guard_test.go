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

// addSecondRecipient wraps the space's key for a fresh X25519 key too, so a
// rotation has someone to revoke while this device remains. It returns the
// recipient id to revoke.
func (h *psHarness) addSecondRecipient(spaceID string) string {
	h.t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	recip, err := psclient.ParseRecipient(encryption.FormatPublicKey(priv.PublicKey().Bytes()))
	if err != nil {
		h.t.Fatal(err)
	}
	key, ok := h.mgr.SpaceKey(spaceID)
	if !ok {
		h.t.Fatalf("space %s is not open on the harness manager", spaceID)
	}
	wrapped, err := encryption.Seal(key, recip.Key)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.RewrapKeys(h.ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: recip.ID, Wrapped: wrapped}}); err != nil {
		h.t.Fatal(err)
	}
	return recip.ID
}

func (h *psHarness) rotate(spaceID, revoke string) (spaceRotateView, error) {
	h.t.Helper()
	cust, err := custody.Select(custody.Options{DeviceDir: h.deviceDir})
	if err != nil {
		h.t.Fatal(err)
	}
	return rotateSpace(h.ctx, h.client, cust, spaceID, []string{revoke})
}

// Rotation re-snapshots the log as a playlist and compacts it, which destroys
// any other CRDT's space (#698). It must refuse those spaces outright, and leave
// them exactly as they were: the same changes, the same wrapped keys.
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
		if len(view.Revoked) != 1 || view.Revoked[0] != other || view.SnapshotID == "" {
			t.Errorf("rotate view = %+v", view)
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
