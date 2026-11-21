package replication_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// putChain writes n linked changes into a space and returns them, oldest first.
func putChain(t *testing.T, s *store.Store, spaceID string, n int, tag byte) []protocol.EncryptedChange {
	t.Helper()
	var out []protocol.EncryptedChange
	var parents []string
	if heads, err := s.HeadsFor(context.Background(), spaceID); err == nil {
		parents = heads
	}
	for i := 0; i < n; i++ {
		c, err := protocol.NewChange(spaceID, parents, []byte{tag, byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutChange(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
		parents = []string{c.ChangeID}
	}
	return out
}

func emptySpace(t *testing.T, s *store.Store) string {
	t.Helper()
	sp, err := s.PutSpace(context.Background(), mustUUID(t), spaces.KindPersonal)
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

func held(t *testing.T, s *store.Store, spaceID string) map[string]bool {
	t.Helper()
	cs, err := s.ChangesFor(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, c := range cs {
		out[c.ChangeID] = true
	}
	return out
}

// TestAuditRepairsATargetMissingAnAncestor is #682 item 2. A target that lost an
// ancestor before topological order shipped offers the child as a head, so the
// heads-based push marks the ancestor known through it on every pass and never
// resends it. The audit asks the target what it holds and resends the hole.
//
// SABOTAGE: skip auditSpace in trustSpace, and the ancestor stays missing.
func TestAuditRepairsATargetMissingAnAncestor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, target := newStore(t), newStore(t)
	space := emptySpace(t, local)
	chain := putChain(t, local, space, 3, 'a') // a0 <- a1 <- a2

	// The damage: the target holds a0 and a2, not a1.
	if _, err := target.PutSpace(ctx, space, spaces.KindPersonal); err != nil {
		t.Fatal(err)
	}
	for _, c := range []protocol.EncryptedChange{chain[0], chain[2]} {
		if err := target.PutChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if missing := protocol.Missing(chain, protocol.Heads([]protocol.EncryptedChange{chain[0], chain[2]})); len(missing) != 0 {
		t.Fatalf("precondition: the heads-based push should see nothing missing, saw %d", len(missing))
	}

	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}
	out := replication.Reconcile(ctx, local, storePusher{target: target}, []replication.Target{tgt}, nil, nil)
	if out[0].Err != nil || out[0].Pushed != 1 {
		t.Fatalf("reconcile = %+v, want the one missing ancestor resent", out[0])
	}
	if !held(t, target, space)[chain[1].ChangeID] {
		t.Fatal("the audit did not resend the missing ancestor")
	}
	acks, _ := local.PeerAcks(ctx, space)
	if a := acks["peer-b"]; len(a.Frontier) != 1 || a.Frontier[0] != chain[2].ChangeID {
		t.Fatalf("ack = %+v, want peer-b acked at the head %s", acks, chain[2].ChangeID)
	}
}

// TestAckIsRecordedOnlyWhenEveryChangeIsConfirmed: an audit cut off mid-repair
// records no acknowledgement, so compaction cannot treat a half-repaired target
// as holding the changes it never received.
func TestAckIsRecordedOnlyWhenEveryChangeIsConfirmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, target := newStore(t), newStore(t)
	space := emptySpace(t, local)
	putChain(t, local, space, 3, 'b')
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}

	// Every change push fails: the space replicates nothing and the audit, had
	// it run, could not have completed either.
	allow := 0
	out := replication.Reconcile(ctx, local, flakyPusher{storePusher: storePusher{target: target}, allow: &allow}, []replication.Target{tgt}, nil, nil)
	if out[0].Err == nil {
		t.Fatal("a reconcile that pushed nothing reported success")
	}
	if acks, _ := local.PeerAcks(ctx, space); len(acks) != 0 {
		t.Fatalf("an ack was recorded for a target that confirmed nothing: %+v", acks)
	}
}

// TestPinnedBaseReplicatesToASibling is #681 item 3: compaction's base reaches
// every trusted Full Peer, pinned there too, so losing the compacting node does
// not lose the only recoverable copy of the compacted range. And compaction
// drops nothing until that sibling has confirmed holding the base.
//
// SABOTAGE: stop pushing the base in replicateSpace (or pinning it in
// trustSpace), and the sibling's base assertion fires; and compaction, waiting
// on an ack that names the base, never drops.
func TestPinnedBaseReplicatesToASibling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := newStore(t), newStore(t)
	space := emptySpace(t, a)
	chain := putChain(t, a, space, 3, 'c')
	base, _ := protocol.NewSnapshot(space, []string{chain[2].ChangeID}, []byte("OPAQUE-BASE"))
	if err := a.PutSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}
	peers := []string{"peer-b"}

	// First call: the base is pinned, peer-b has not confirmed it, nothing goes.
	res, err := a.CompactChanges(ctx, space, base.SnapshotID, peers)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped != 0 || len(res.Waiting) != 1 {
		t.Fatalf("first compaction = %+v, want it waiting for peer-b", res)
	}

	// An audited reconcile carries the base and its pin, and records the ack.
	if out := replication.Reconcile(ctx, a, storePusher{target: b}, []replication.Target{tgt}, nil, nil); out[0].Err != nil {
		t.Fatal(out[0].Err)
	}
	got, ok, err := b.Base(ctx, space)
	if err != nil || !ok || got.SnapshotID != base.SnapshotID {
		t.Fatalf("the sibling's base = %+v ok=%v err=%v, want the pinned %s", got.SnapshotID, ok, err, base.SnapshotID)
	}
	acks, _ := a.PeerAcks(ctx, space)
	if acks["peer-b"].Base != base.SnapshotID {
		t.Fatalf("peer-b's ack names base %q, want %s", acks["peer-b"].Base, base.SnapshotID)
	}

	// Second call: now it is proven, and the subsumed changes go.
	res, err = a.CompactChanges(ctx, space, base.SnapshotID, peers)
	if err != nil || res.Dropped != 3 {
		t.Fatalf("second compaction = %+v err=%v, want 3 dropped", res, err)
	}

	// Bounded history holds in the other direction too: b's audit of a counts
	// a's base as covering what a compacted, and resends none of it (#682).
	back := replication.Target{Peer: mtls.Peer{PeerID: "peer-a"}}
	out := replication.Reconcile(ctx, b, storePusher{target: a}, []replication.Target{back}, nil, nil)
	if out[0].Err != nil || out[0].Pushed != 0 {
		t.Fatalf("b -> a reconcile = %+v, want nothing resent", out[0])
	}
	if n := len(held(t, a, space)); n != 0 {
		t.Fatalf("a holds %d changes again after b's reconcile; compaction was undone", n)
	}
}

// TestASiblingAheadKeepsItsBase: a target whose base this node's does not cover
// refuses the pin, and the reconcile treats that as "behind", not a failure.
func TestASiblingAheadKeepsItsBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := newStore(t), newStore(t)
	space := emptySpace(t, a)
	chain := putChain(t, a, space, 3, 'd')
	old, _ := protocol.NewSnapshot(space, []string{chain[0].ChangeID}, []byte("OPAQUE-OLD"))
	newer, _ := protocol.NewSnapshot(space, []string{chain[2].ChangeID}, []byte("OPAQUE-NEW"))
	for _, s := range []protocol.EncryptedSnapshot{old, newer} {
		if err := a.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}
	if out := replication.Reconcile(ctx, a, storePusher{target: b}, []replication.Target{tgt}, nil, nil); out[0].Err != nil {
		t.Fatal(out[0].Err)
	}
	if _, err := b.PinBase(ctx, space, newer.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PinBase(ctx, space, old.SnapshotID); err != nil {
		t.Fatal(err)
	}
	out := replication.Reconcile(ctx, a, storePusher{target: b}, []replication.Target{tgt}, nil, nil)
	if out[0].Err != nil {
		t.Fatalf("a refused pin failed the space: %v", out[0].Err)
	}
	if got, _, _ := b.Base(ctx, space); got.SnapshotID != newer.SnapshotID {
		t.Fatalf("the sibling's base moved back to %s", got.SnapshotID)
	}
	if acks, _ := a.PeerAcks(ctx, space); acks["peer-b"].Base != newer.SnapshotID {
		t.Fatalf("the ack should name the base peer-b reported, got %q", acks["peer-b"].Base)
	}
}

// noTrustPusher is a target that predates snapshot trust: it has no held or base
// route, so both answer an error.
type noTrustPusher struct{ storePusher }

var errNoRoute = errors.New("404 page not found")

func (noTrustPusher) PinBase(context.Context, replication.Target, string, string) error {
	return errNoRoute
}

func (noTrustPusher) Held(context.Context, replication.Target, string, []string) (replication.HeldAnswer, error) {
	return replication.HeldAnswer{}, errNoRoute
}

// TestATargetWithoutTheAuditRouteStillReplicates: a sibling that predates the
// held route still converges on every change; it simply never acks, so nothing
// is ever compacted against it.
func TestATargetWithoutTheAuditRouteStillReplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, target := newStore(t), newStore(t)
	space := emptySpace(t, local)
	chain := putChain(t, local, space, 2, 'e')
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-old"}}
	out := replication.Reconcile(ctx, local, noTrustPusher{storePusher{target: target}}, []replication.Target{tgt}, nil, nil)
	if out[0].Err != nil || out[0].Pushed != 2 {
		t.Fatalf("reconcile = %+v, want both changes pushed despite the missing audit route", out[0])
	}
	if !held(t, target, space)[chain[1].ChangeID] {
		t.Fatal("the target did not receive the changes")
	}
	if acks, _ := local.PeerAcks(ctx, space); len(acks) != 0 {
		t.Fatalf("an ack was recorded for a target that confirmed nothing: %+v", acks)
	}
}

// countingPusher counts held queries, to observe when a Reconciler audits.
type countingPusher struct {
	storePusher
	asked *int
}

func (p countingPusher) Held(ctx context.Context, t replication.Target, s string, ids []string) (replication.HeldAnswer, error) {
	*p.asked++
	return p.storePusher.Held(ctx, t, s, ids)
}

type staticPeers []replication.Target

func (s staticPeers) FullPeerTargets(context.Context) ([]replication.Target, error) { return s, nil }

// TestReconcilerAuditsOnStartThenOnItsInterval: the beat's reconciler audits each
// (peer, space) on its first pass and then once per interval, on its clock.
func TestReconcilerAuditsOnStartThenOnItsInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, target := newStore(t), newStore(t)
	space := emptySpace(t, local)
	putChain(t, local, space, 1, 'f')
	asked := 0
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	r := replication.NewReconciler(local, countingPusher{storePusher{target: target}, &asked},
		staticPeers{{Peer: mtls.Peer{PeerID: "peer-b"}}}, nil, nil).
		AuditEvery(time.Hour, func() time.Time { return now })

	for i, step := range []struct {
		advance time.Duration
		want    int
	}{{0, 1}, {time.Minute, 1}, {time.Hour, 2}} {
		now = now.Add(step.advance)
		if _, _, err := r.ReconcileAll(ctx); err != nil {
			t.Fatal(err)
		}
		if asked != step.want {
			t.Fatalf("after pass %d, %d audits, want %d", i, asked, step.want)
		}
	}
	ids, err := r.FullPeerIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "peer-b" {
		t.Fatalf("FullPeerIDs = %v, %v", ids, err)
	}
}
