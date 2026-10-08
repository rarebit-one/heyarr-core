package cli

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/encryption"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
)

// deviceRecipientID is the harness device's own wrap recipient id.
func (h *psHarness) deviceRecipientID() string {
	h.t.Helper()
	ds, err := device.NewStore(device.StoreOptions{Dir: h.deviceDir})
	if err != nil {
		h.t.Fatal(err)
	}
	pk, err := ds.LoadEncryptionKey()
	if err != nil {
		h.t.Fatal(err)
	}
	return encryption.FormatPublicKey(pk.PublicKey().Bytes())
}

// wrapFresh seals a fresh space key for the harness device: what a rotation's
// new-key copy is. The peer never opens it.
func (h *psHarness) wrapFresh() []byte {
	h.t.Helper()
	ds, err := device.NewStore(device.StoreOptions{Dir: h.deviceDir})
	if err != nil {
		h.t.Fatal(err)
	}
	pk, err := ds.LoadEncryptionKey()
	if err != nil {
		h.t.Fatal(err)
	}
	k, err := encryption.NewSpaceKey()
	if err != nil {
		h.t.Fatal(err)
	}
	w, err := encryption.Seal(k, pk.PublicKey())
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

// problemOf asserts err is an API refusal with the given status (and, when set,
// problem code).
func problemOf(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an API refusal %d", err, status)
	}
	if apiErr.Status != status {
		t.Fatalf("status = %d, want %d (%v)", apiErr.Status, status, err)
	}
	if code != "" && (apiErr.Problem == nil || apiErr.Problem.Code != code) {
		t.Fatalf("problem = %+v, want code %q", apiErr.Problem, code)
	}
}

// TestSpaceKeyEpochOverTheAPI drives ADR-0103's device surface through the real
// router and auth: rotation needs `admin`; a rotation moves the epoch, records
// the opaque history row, and serves only the new wraps; a racing rotation and a
// wrap at a superseded epoch are both 409s with a stable code.
func TestSpaceKeyEpochOverTheAPI(t *testing.T) {
	t.Run("rotation needs admin", func(t *testing.T) {
		h := newPSHarness(t) // read + write only
		spaceID := h.createSpace()
		recip := h.deviceRecipientID()
		_, err := h.client.RotateKey(h.ctx, spaceID, 0, []byte("prev"),
			[]apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh()}}, nil)
		problemOf(t, err, http.StatusForbidden, "")
		keys, err := h.client.SpaceKeys(h.ctx, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		if keys.KeyEpoch != 0 {
			t.Fatalf("a refused rotation moved the epoch to %d", keys.KeyEpoch)
		}
	})

	t.Run("rotate, then refuse the stale", func(t *testing.T) {
		h := newPSHarness(t, auth.ScopeAdmin)
		spaceID := h.createSpace()
		recip := h.deviceRecipientID()

		before, err := h.client.SpaceKeys(h.ctx, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		if before.KeyEpoch != 0 || len(before.WrappedKeys) != 1 || before.WrappedKeys[0].Epoch != 0 {
			t.Fatalf("a fresh space = %+v, want epoch 0 with one epoch-0 wrap", before)
		}
		if h0, err := h.client.KeyHistory(h.ctx, spaceID); err != nil || len(h0) != 0 {
			t.Fatalf("a fresh space's history = %v, %v; want empty", h0, err)
		}

		newWrap := h.wrapFresh()
		epoch, err := h.client.RotateKey(h.ctx, spaceID, 0, []byte("k0-sealed-under-k1"),
			[]apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: newWrap}}, nil)
		if err != nil {
			t.Fatalf("RotateKey: %v", err)
		}
		if epoch != 1 {
			t.Fatalf("RotateKey returned epoch %d, want 1", epoch)
		}
		after, err := h.client.SpaceKeys(h.ctx, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		if after.KeyEpoch != 1 || len(after.WrappedKeys) != 1 || after.WrappedKeys[0].Epoch != 1 ||
			string(after.WrappedKeys[0].Wrapped) != string(newWrap) {
			t.Fatalf("after rotation = %+v, want epoch 1 serving only the new wrap", after)
		}
		history, err := h.client.KeyHistory(h.ctx, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		if len(history) != 1 || history[0].Epoch != 1 || string(history[0].SealedPrev) != "k0-sealed-under-k1" {
			t.Fatalf("history = %+v, want the one opaque row", history)
		}

		// A rotation racing from the old epoch loses.
		_, err = h.client.RotateKey(h.ctx, spaceID, 0, []byte("fork"),
			[]apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh()}}, nil)
		problemOf(t, err, http.StatusConflict, psapi.CodeKeyEpochConflict)

		// Adding a recipient with a copy of the superseded key is refused...
		err = h.client.RewrapKeys(h.ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh()}})
		problemOf(t, err, http.StatusConflict, psapi.CodeKeyEpochStale)
		// ...as is one claiming an epoch the space has not reached...
		err = h.client.RewrapKeys(h.ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh(), Epoch: 2}})
		problemOf(t, err, http.StatusConflict, psapi.CodeKeyEpochAhead)
		// ...and one at the current epoch lands.
		if err := h.client.RewrapKeys(h.ctx, spaceID, []apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: newWrap, Epoch: 1}}); err != nil {
			t.Fatalf("a wrap at the current epoch: %v", err)
		}
	})

	t.Run("malformed rotations are 400s", func(t *testing.T) {
		h := newPSHarness(t, auth.ScopeAdmin)
		spaceID := h.createSpace()
		recip := h.deviceRecipientID()
		cases := []struct {
			name   string
			sealed []byte
			wraps  []apiclient.WrappedKeyInput
		}{
			{"no wraps", []byte("p"), nil},
			{"no sealed previous key", nil, []apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh()}}},
			{"a wrap that names an epoch", []byte("p"), []apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh(), Epoch: 1}}},
		}
		for _, tc := range cases {
			_, err := h.client.RotateKey(h.ctx, spaceID, 0, tc.sealed, tc.wraps, nil)
			if err == nil {
				t.Fatalf("%s: accepted", tc.name)
			}
			problemOf(t, err, http.StatusBadRequest, "")
		}
		_, err := h.client.RotateKey(h.ctx, "0199ffff-0000-7000-8000-000000000000", 0, []byte("p"),
			[]apiclient.WrappedKeyInput{{Recipient: recip, Wrapped: h.wrapFresh()}}, nil)
		problemOf(t, err, http.StatusNotFound, "")
		if _, err := h.client.KeyHistory(h.ctx, "0199ffff-0000-7000-8000-000000000000"); err == nil {
			t.Fatal("the history of an unknown space was served")
		} else {
			problemOf(t, err, http.StatusNotFound, "")
		}
	})
}
