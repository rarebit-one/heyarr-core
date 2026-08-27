package store_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// chain writes c1<-c2<-c3 plus a sibling c4 (parent c1) into a space, returning
// their ids. c4 stands in for a partitioned peer's concurrent branch.
func chain(t *testing.T, s interface {
	PutChange(context.Context, protocol.EncryptedChange) error
}, spaceID string,
) (c1, c2, c3, c4 string) {
	t.Helper()
	ctx := context.Background()
	mk := func(parents []string, ct string) protocol.EncryptedChange {
		ch, err := protocol.NewChange(spaceID, parents, []byte(ct))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutChange(ctx, ch); err != nil {
			t.Fatal(err)
		}
		return ch
	}
	a := mk(nil, "OPAQUE-1")
	b := mk([]string{a.ChangeID}, "OPAQUE-2")
	c := mk([]string{b.ChangeID}, "OPAQUE-3")
	d := mk([]string{a.ChangeID}, "OPAQUE-4")
	return a.ChangeID, b.ChangeID, c.ChangeID, d.ChangeID
}

func TestPutAndLatestSnapshot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, err := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LatestSnapshotFor(ctx, sp.ID); ok {
		t.Fatal("a fresh space has no snapshot")
	}
	c1, c2, c3, _ := chain(t, s, sp.ID)
	_ = c1
	_ = c2
	snap, err := protocol.NewSnapshot(sp.ID, []string{c3}, []byte("OPAQUE-SNAPSHOT"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	got, ok, err := s.LatestSnapshotFor(ctx, sp.ID)
	if err != nil || !ok || got.SnapshotID != snap.SnapshotID {
		t.Fatalf("LatestSnapshotFor = %+v ok=%v err=%v", got, ok, err)
	}
	// A forged snapshot id is refused before storage.
	forged := protocol.EncryptedSnapshot{SpaceID: sp.ID, SnapshotID: "blake3:deadbeef", Ciphertext: []byte("xxxxxxxxxxxxxxxx")}
	if err := s.PutSnapshot(ctx, forged); err == nil {
		t.Fatal("a forged snapshot id should be refused")
	}
}

// TestCompactionRespectsTheAcknowledgedFrontier is the safety of compaction: a
// change is dropped only if the snapshot subsumes it AND it is within the frontier
// EVERY replica has acknowledged — so a change a partitioned peer still needs
// survives.
//
// SABOTAGE (the reviewer's break): drop the acked-frontier condition in
// CompactChanges and compact by snapshot subsumption alone — then the
// behind-peer case below drops c2/c3, which a peer that has only acknowledged c1
// (and not the snapshot) would never receive, and the "still there" assertion
// fires.
func TestCompactionRespectsTheAcknowledgedFrontier(t *testing.T) {
	ctx := context.Background()

	// Case A: every replica has acknowledged up to c3. Compaction drops c1,c2,c3
	// (subsumed by the snapshot and acknowledged) but keeps c4, the sibling branch
	// the snapshot does not subsume.
	{
		s := newStore(t)
		sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
		c1, c2, c3, c4 := chain(t, s, sp.ID)
		snap, _ := protocol.NewSnapshot(sp.ID, []string{c3}, []byte("OPAQUE-SNAP-A"))
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		dropped, err := s.CompactChanges(ctx, sp.ID, []string{c3})
		if err != nil {
			t.Fatalf("CompactChanges: %v", err)
		}
		if dropped != 3 {
			t.Fatalf("dropped %d, want 3 (c1,c2,c3)", dropped)
		}
		remaining := ids(t, s, sp.ID)
		if has(remaining, c1) || has(remaining, c2) || has(remaining, c3) {
			t.Fatalf("a subsumed+acknowledged change survived: %v", remaining)
		}
		if !has(remaining, c4) {
			t.Fatalf("the un-subsumed sibling branch c4 was dropped: %v", remaining)
		}
	}

	// Case B: a peer has acknowledged only c1 (it is behind, or partitioned, and
	// does NOT hold the snapshot). Compaction must drop only c1 — c2 and c3 must
	// survive so the behind peer can still receive and converge.
	{
		s := newStore(t)
		sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
		c1, c2, c3, _ := chain(t, s, sp.ID)
		snap, _ := protocol.NewSnapshot(sp.ID, []string{c3}, []byte("OPAQUE-SNAP-B"))
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		dropped, err := s.CompactChanges(ctx, sp.ID, []string{c1})
		if err != nil {
			t.Fatalf("CompactChanges: %v", err)
		}
		if dropped != 1 {
			t.Fatalf("dropped %d, want 1 (only c1)", dropped)
		}
		remaining := ids(t, s, sp.ID)
		if !has(remaining, c2) || !has(remaining, c3) {
			t.Fatalf("a change a behind peer still needs was compacted: %v", remaining)
		}
	}
}

func ids(t *testing.T, s interface {
	ChangesFor(context.Context, string) ([]protocol.EncryptedChange, error)
}, spaceID string,
) []string {
	t.Helper()
	cs, err := s.ChangesFor(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ChangeID
	}
	return out
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
