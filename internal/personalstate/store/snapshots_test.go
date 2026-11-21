package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
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
	t.Parallel()
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

// TestCompactionRespectsEveryPeersAck is the safety of compaction (#681): a
// change is dropped only if the named snapshot subsumes it AND it is in the
// durably acknowledged history of EVERY trusted Full Peer whose ack names that
// same base. A frontier the caller supplies is no longer proof of anything.
//
// SABOTAGE (the reviewer's break): drop the acked-history condition in
// CompactChanges and compact by subsumption alone. The behind-peer case then
// drops c2 and c3, which a peer that confirmed only c1 would never receive, and
// its "still there" assertion fires. Drop the base condition instead, and the
// first case compacts before any peer holds the base.
func TestCompactionRespectsEveryPeersAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := func(t *testing.T) (*store.Store, string, [4]string, protocol.EncryptedSnapshot) {
		s := newStore(t)
		sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
		c1, c2, c3, c4 := chain(t, s, sp.ID)
		snap, _ := protocol.NewSnapshot(sp.ID, []string{c3}, []byte("OPAQUE-SNAP"))
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return s, sp.ID, [4]string{c1, c2, c3, c4}, snap
	}

	t.Run("a peer without an ack naming the base holds compaction back", func(t *testing.T) {
		s, space, c, snap := setup(t)
		// peer-b acked everything, but before this base existed.
		if err := s.RecordPeerAck(ctx, space, "peer-b", []string{c[2], c[3]}, ""); err != nil {
			t.Fatal(err)
		}
		res, err := s.CompactChanges(ctx, space, snap.SnapshotID, []string{"peer-b"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Dropped != 0 || !res.Pinned || len(res.Waiting) != 1 || res.Waiting[0] != "peer-b" {
			t.Fatalf("compaction = %+v, want the base pinned, nothing dropped, waiting for peer-b", res)
		}
		if base, ok, _ := s.Base(ctx, space); !ok || base.SnapshotID != snap.SnapshotID {
			t.Fatalf("the named snapshot was not pinned as the base: %+v ok=%v", base, ok)
		}
	})

	t.Run("every peer acked the base: the subsumed and acked changes go", func(t *testing.T) {
		s, space, c, snap := setup(t)
		if _, err := s.PinBase(ctx, space, snap.SnapshotID); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"peer-b", "peer-c"} {
			if err := s.RecordPeerAck(ctx, space, p, []string{c[2], c[3]}, snap.SnapshotID); err != nil {
				t.Fatal(err)
			}
		}
		res, err := s.CompactChanges(ctx, space, snap.SnapshotID, []string{"peer-b", "peer-c"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Dropped != 3 || len(res.Waiting) != 0 {
			t.Fatalf("compaction = %+v, want c1..c3 dropped", res)
		}
		remaining := ids(t, s, space)
		if has(remaining, c[0]) || has(remaining, c[1]) || has(remaining, c[2]) || !has(remaining, c[3]) {
			t.Fatalf("remaining %v, want only the sibling branch c4", remaining)
		}
	})

	t.Run("a peer that confirmed only c1 keeps c2 and c3", func(t *testing.T) {
		s, space, c, snap := setup(t)
		if _, err := s.PinBase(ctx, space, snap.SnapshotID); err != nil {
			t.Fatal(err)
		}
		_ = s.RecordPeerAck(ctx, space, "peer-b", []string{c[2], c[3]}, snap.SnapshotID)
		_ = s.RecordPeerAck(ctx, space, "peer-c", []string{c[0]}, snap.SnapshotID)
		res, err := s.CompactChanges(ctx, space, snap.SnapshotID, []string{"peer-b", "peer-c"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Dropped != 1 {
			t.Fatalf("dropped %d, want 1 (only c1)", res.Dropped)
		}
		if remaining := ids(t, s, space); !has(remaining, c[1]) || !has(remaining, c[2]) {
			t.Fatalf("a change a behind peer still needs was compacted: %v", remaining)
		}
	})

	t.Run("an ack from a peer that is no longer trusted proves nothing", func(t *testing.T) {
		s, space, c, snap := setup(t)
		_ = s.RecordPeerAck(ctx, space, "peer-gone", []string{c[2]}, snap.SnapshotID)
		res, err := s.CompactChanges(ctx, space, snap.SnapshotID, []string{"peer-b"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Dropped != 0 || len(res.Waiting) != 1 {
			t.Fatalf("compaction = %+v, want it waiting for peer-b", res)
		}
	})

	t.Run("an unknown snapshot is refused", func(t *testing.T) {
		s, space, _, _ := setup(t)
		other, _ := protocol.NewSnapshot(space, nil, []byte("NEVER-PUSHED"))
		if _, err := s.CompactChanges(ctx, space, other.SnapshotID, nil); !errors.Is(err, store.ErrUnknownSnapshot) {
			t.Fatalf("compacting against an unheld snapshot: err = %v, want ErrUnknownSnapshot", err)
		}
	})
}

// TestBaseOnlyMovesForward: a pin that does not provably cover the current base
// is refused, so a cold start can never be steered behind a compaction (#681).
func TestBaseOnlyMovesForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
	c1, _, c3, c4 := chain(t, s, sp.ID)
	at := func(frontier []string, tag string) protocol.EncryptedSnapshot {
		snap, _ := protocol.NewSnapshot(sp.ID, frontier, []byte(tag))
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return snap
	}
	early, base, sibling, merged := at([]string{c1}, "EARLY"), at([]string{c3}, "BASE"), at([]string{c4}, "SIBLING"), at([]string{c3, c4}, "MERGED")

	if pinned, err := s.PinBase(ctx, sp.ID, base.SnapshotID); err != nil || !pinned {
		t.Fatalf("first pin: pinned=%v err=%v", pinned, err)
	}
	if pinned, err := s.PinBase(ctx, sp.ID, base.SnapshotID); err != nil || pinned {
		t.Fatalf("re-pin: pinned=%v err=%v, want an idempotent no-op", pinned, err)
	}
	for _, back := range []protocol.EncryptedSnapshot{early, sibling} {
		if _, err := s.PinBase(ctx, sp.ID, back.SnapshotID); !errors.Is(err, store.ErrBaseNotCovering) {
			t.Fatalf("pinning a snapshot that does not cover the base: err = %v, want ErrBaseNotCovering", err)
		}
	}
	if pinned, err := s.PinBase(ctx, sp.ID, merged.SnapshotID); err != nil || !pinned {
		t.Fatalf("pinning a covering snapshot: pinned=%v err=%v", pinned, err)
	}
}

// TestColdStartPrefersTheBase is #681 item 2: after a compaction, the snapshot a
// cold-starting device is offered is the newest one that covers the pinned base,
// never merely the newest to arrive.
//
// SABOTAGE: make ColdStartSnapshot return LatestSnapshotFor, and the "late
// stale snapshot" case below offers the stale one.
func TestColdStartPrefersTheBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
	c1, _, c3, c4 := chain(t, s, sp.ID)
	push := func(frontier []string, tag string) protocol.EncryptedSnapshot {
		snap, _ := protocol.NewSnapshot(sp.ID, frontier, []byte(tag))
		if err := s.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return snap
	}
	offered := func() string {
		got, ok, err := s.ColdStartSnapshot(ctx, sp.ID)
		if err != nil || !ok {
			t.Fatalf("ColdStartSnapshot: ok=%v err=%v", ok, err)
		}
		return got.SnapshotID
	}

	first := push([]string{c1}, "FIRST")
	if offered() != first.SnapshotID {
		t.Fatal("with no base, the newest snapshot is offered")
	}
	base := push([]string{c3}, "BASE")
	if res, err := s.CompactChanges(ctx, sp.ID, base.SnapshotID, nil); err != nil || res.Dropped != 3 {
		t.Fatalf("compaction = %+v err=%v", res, err)
	}
	stale := push([]string{c1}, "LATE-STALE") // an offline member's old snapshot arrives last
	_ = stale
	if got := offered(); got != base.SnapshotID {
		t.Fatalf("offered %s, want the base %s over a late snapshot that predates it", got, base.SnapshotID)
	}
	// A tail change on top of the compacted base, then a snapshot at it: that one
	// provably covers the base (the tail names c3 as its parent) and is offered.
	tail, _ := protocol.NewChange(sp.ID, []string{c3, c4}, []byte("OPAQUE-TAIL"))
	if err := s.PutChange(ctx, tail); err != nil {
		t.Fatal(err)
	}
	newer := push([]string{tail.ChangeID}, "NEWER")
	if got := offered(); got != newer.SnapshotID {
		t.Fatalf("offered %s, want the newer snapshot %s that covers the base", got, newer.SnapshotID)
	}
	if b, ok, _ := s.Base(ctx, sp.ID); !ok || b.SnapshotID != base.SnapshotID {
		t.Fatal("offering a newer snapshot must not move the base")
	}
}

// TestHeldChangesConfirmsRows: the target answers which offered ids it holds as
// rows, in the order asked, once each, across more ids than one SQL batch.
func TestHeldChangesConfirmsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStore(t)
	sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
	c1, c2, _, c4 := chain(t, s, sp.ID)
	ask := []string{c4, "blake3:" + strings.Repeat("0", 64), c1, c1}
	for i := 0; i < 1200; i++ {
		ask = append(ask, fmt.Sprintf("blake3:%064x", i))
	}
	ask = append(ask, c2)
	got, err := s.HeldChanges(ctx, sp.ID, ask)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{c4, c1, c2}; !slices.Equal(got, want) {
		t.Fatalf("HeldChanges = %v, want %v", got, want)
	}
}

// TestRecordPeerAckEmitsOnlyOnChange: an unchanged acknowledgement is not
// rewritten, so a converged fleet's audits stay quiet (Invariant 7 still holds
// for every real transition).
func TestRecordPeerAckEmitsOnlyOnChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, log := newStoreWithLog(t)
	sp, _ := s.PutSpace(ctx, mustUUIDStr(t), spaces.KindPersonal)
	count := func() int {
		evs, err := log.Since(ctx, 0, []string{events.TypeStatePeerAcked}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	for _, f := range [][]string{{"blake3:b", "blake3:a"}, {"blake3:a", "blake3:b"}} {
		if err := s.RecordPeerAck(ctx, sp.ID, "peer-b", f, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("%d ack events for one unchanged record, want 1", n)
	}
	if err := s.RecordPeerAck(ctx, sp.ID, "peer-b", []string{"blake3:a"}, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d ack events after a change, want 2", n)
	}
	acks, _ := s.PeerAcks(ctx, sp.ID)
	if a := acks["peer-b"]; len(a.Frontier) != 1 || a.Frontier[0] != "blake3:a" {
		t.Fatalf("PeerAcks = %+v", acks)
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
