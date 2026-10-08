package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// rotatingOpen opens the space for vaultPutFresh and, on each of the first
// `rotations` opens, re-keys the space right after — a rotation landing
// between the writer opening the space and publishing its change (#712).
func (h *psHarness) rotatingOpen(spaceID string, rotations int, opens *int) func(context.Context) (*psclient.Manager, error) {
	return func(ctx context.Context) (*psclient.Manager, error) {
		*opens++
		mgr, err := openSpace(ctx, h.client, h.custody(), spaceID)
		if err != nil {
			return nil, err
		}
		if *opens <= rotations {
			if _, err := h.rekey(spaceID); err != nil {
				h.t.Fatalf("rekey: %v", err)
			}
		}
		return mgr, nil
	}
}

// TestVaultPutReSealsAfterARotation: a write sealed under epoch 0 while the
// space rotates to epoch 1 is refused by the peer, re-sealed under the new key,
// and lands once. The one change the drive holds is not readable with the
// epoch-0 key alone, so the late, old-key seal never published.
func TestVaultPutReSealsAfterARotation(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	data := bytes.Repeat([]byte("sealed while the space rotated\n"), 5000)

	var opens int
	view, err := vaultPutFresh(h.ctx, h.client, h.rotatingOpen(spaceID, 1, &opens),
		spaceID, "docs/a.txt", bytes.NewReader(data), time.Now().Unix())
	if err != nil {
		t.Fatalf("vaultPutFresh: %v", err)
	}
	if opens != 2 {
		t.Fatalf("opened the space %d times, want 2 (one re-seal)", opens)
	}
	if view.Size != int64(len(data)) {
		t.Fatalf("view.Size = %d, want %d", view.Size, len(data))
	}
	changes, err := h.client.Changes(h.ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].ChangeID != view.ChangeID {
		t.Fatalf("changes = %d, want only the re-sealed write", len(changes))
	}
	// h.mgr holds only the epoch-0 key it created the space with.
	if _, err := statesync.DecodeAllChanges[crdt.DriveChange](h.mgr, changes); err == nil {
		t.Fatal("the recorded change opens under the superseded epoch-0 key")
	}
	if got := h.vaultPull(spaceID, "docs/a.txt"); !bytes.Equal(got, data) {
		t.Fatalf("pulled %d bytes, want %d identical", len(got), len(data))
	}
}

// TestVaultPutGivesUpAfterTwoRotations: a space rotating under both attempts
// ends in an error with nothing recorded in the drive.
func TestVaultPutGivesUpAfterTwoRotations(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()

	var opens int
	_, err := vaultPutFresh(h.ctx, h.client, h.rotatingOpen(spaceID, 2, &opens),
		spaceID, "docs/a.txt", bytes.NewReader([]byte("never recorded")), time.Now().Unix())
	if !apiclient.IsChangeKeyEpochMismatch(err) {
		t.Fatalf("vaultPutFresh = %v, want a change_key_epoch_mismatch refusal", err)
	}
	if opens != maxVaultSealAttempts {
		t.Fatalf("opened the space %d times, want %d", opens, maxVaultSealAttempts)
	}
	if changes, err := h.client.Changes(h.ctx, spaceID); err != nil || len(changes) != 0 {
		t.Fatalf("changes = %d (%v), want none", len(changes), err)
	}
}
