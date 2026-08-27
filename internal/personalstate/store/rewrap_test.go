package store_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// TestDeleteWrappedKeyRemovesOnlyThatRecipient: revoking one recipient's copy
// leaves the others, and is idempotent.
func TestDeleteWrappedKeyRemovesOnlyThatRecipient(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)
	_, aID := device(t)
	_, bID := device(t)
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, bID, []byte("b")); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteWrappedKey(ctx, sp.ID, bID); err != nil {
		t.Fatalf("DeleteWrappedKey: %v", err)
	}
	keys, err := s.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Recipient != aID {
		t.Fatalf("after revoking B, keys = %v, want only A", keys)
	}
	// Idempotent: revoking again is a no-op, not an error.
	if err := s.DeleteWrappedKey(ctx, sp.ID, bID); err != nil {
		t.Fatalf("second DeleteWrappedKey errored: %v", err)
	}
}
