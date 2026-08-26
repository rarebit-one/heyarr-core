package crdt

import (
	"math"
	"math/rand"
	"testing"
)

// Adversarial synthetic tests for the playlist CRDT: cases the hand-written
// suite does not exercise — arbitrary hostile Change values, not the well-formed
// ones Add/Remove produce.
//
// The threat model is sharper than "network reorders changes". A Change is an
// UNSIGNED, public struct, and the infrastructure transports it opaquely (§42) —
// it cannot validate a change it cannot read. So a change reaches Apply from an
// authorised-but-MALICIOUS device that crafted any bytes it liked: a remove
// observing tags never added, a colliding tag, a Lamport counter at the integer
// ceiling. The property the milestone rests on — every replica converges to the
// same state regardless of arrival order (§43) — must hold, degrade predictably,
// or its exact boundary must be pinned by a test. These tests find that boundary.

// applyIn folds changes into a fresh state in a given order and returns it — the
// tool for asserting order-independence over hostile inputs.
func applyIn(changes []Change) *State {
	s := New()
	s.Apply(changes...)
	return s
}

// shuffled returns a deterministic permutation of changes for seed — deterministic
// so a failure reproduces from the source (math/rand with a fixed seed, never a
// wall clock).
func shuffled(changes []Change, seed int64) []Change {
	out := append([]Change(nil), changes...)
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestConvergesUnderHostileChangeOrdering: a pile of arbitrary, unrelated changes
// — adds, removes of live tags, removes of tags never added — applied in 200
// random orders all converge to one byte-identical state. This is the semilattice
// property under inputs Add/Remove would never emit.
func TestConvergesUnderHostileChangeOrdering(t *testing.T) {
	t.Parallel()
	// Build a hostile changeset with distinct tags (the legitimate invariant).
	var changes []Change
	tags := make([]Tag, 0, 20)
	for i := 0; i < 20; i++ {
		tag := Tag(string(rune('a'+i)) + "-tag")
		tags = append(tags, tag)
		changes = append(changes, Change{
			Op: OpAdd, ItemID: "item-" + string(rune('a'+(i%5))),
			Tag: tag, Order: OrderKey{Counter: uint64(i % 7), Tag: tag},
		})
	}
	// Removes that observe real tags, plus removes of tags NEVER added.
	changes = append(changes, Change{Op: OpRemove, ItemID: "item-a", Observed: []Tag{tags[0], tags[5]}})
	changes = append(changes, Change{Op: OpRemove, ItemID: "ghost", Observed: []Tag{"never-added-1", "never-added-2"}})

	want := applyIn(changes).Encode()
	for seed := int64(0); seed < 200; seed++ {
		if got := applyIn(shuffled(changes, seed)).Encode(); got != want {
			t.Fatalf("permutation seed=%d diverged:\n got=%s\nwant=%s", seed, got, want)
		}
	}
}

// TestRemoveOfNeverAddedTagIsHarmless: tombstoning a tag that has no add is inert
// — it never surfaces as an item and never panics — and it converges whether it
// arrives before or after unrelated adds.
func TestRemoveOfNeverAddedTagIsHarmless(t *testing.T) {
	t.Parallel()
	add := Change{Op: OpAdd, ItemID: "x", Tag: "real", Order: OrderKey{Counter: 1, Tag: "real"}}
	ghostRemove := Change{Op: OpRemove, ItemID: "x", Observed: []Tag{"phantom"}}

	a := applyIn([]Change{add, ghostRemove})
	b := applyIn([]Change{ghostRemove, add})
	if a.Encode() != b.Encode() {
		t.Fatal("a ghost tombstone made the result order-dependent")
	}
	if ids := a.IDs(); len(ids) != 1 || ids[0] != "x" {
		t.Fatalf("the real item did not survive a ghost tombstone: %v", ids)
	}
}

// TestPreemptiveTombstoneConverges: a remove that observes a tag BEFORE its add
// arrives (a pre-emptive tombstone — an attacker who guessed the tag, or plain
// reordering) converges the same as add-then-remove, and the tag ends dead in
// both. Pinning this proves tombstone/add order does not matter.
func TestPreemptiveTombstoneConverges(t *testing.T) {
	t.Parallel()
	tag := Tag("t1")
	add := Change{Op: OpAdd, ItemID: "x", Tag: tag, Order: OrderKey{Counter: 1, Tag: tag}}
	remove := Change{Op: OpRemove, ItemID: "x", Observed: []Tag{tag}}

	addFirst := applyIn([]Change{add, remove})
	removeFirst := applyIn([]Change{remove, add})
	if addFirst.Encode() != removeFirst.Encode() {
		t.Fatal("pre-emptive tombstone diverged from add-then-remove")
	}
	if len(addFirst.IDs()) != 0 {
		t.Fatalf("the tombstoned tag survived: %v", addFirst.IDs())
	}
}

// TestReplayIsIdempotent: applying the same changeset any number of times equals
// applying it once — a replayed change (a malicious or lossy relay resending) can
// never accumulate.
func TestReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	changes := []Change{
		{Op: OpAdd, ItemID: "a", Tag: "ta", Order: OrderKey{Counter: 1, Tag: "ta"}},
		{Op: OpAdd, ItemID: "b", Tag: "tb", Order: OrderKey{Counter: 2, Tag: "tb"}},
		{Op: OpRemove, ItemID: "a", Observed: []Tag{"ta"}},
	}
	once := applyIn(changes).Encode()

	many := New()
	for i := 0; i < 50; i++ {
		many.Apply(shuffled(changes, int64(i))...)
	}
	if many.Encode() != once {
		t.Fatal("replaying changes changed the state: not idempotent")
	}
}

// TestConcurrentAddRemoveIsAddWins under hostile crafting: a remove that observed
// only tag t1, and a concurrent add of the same item under t2, leaves the item
// present — regardless of order, because the remove tombstones only what it saw.
func TestConcurrentAddRemoveIsAddWins(t *testing.T) {
	t.Parallel()
	add1 := Change{Op: OpAdd, ItemID: "song", Tag: "t1", Order: OrderKey{Counter: 1, Tag: "t1"}}
	remove := Change{Op: OpRemove, ItemID: "song", Observed: []Tag{"t1"}}
	add2 := Change{Op: OpAdd, ItemID: "song", Tag: "t2", Order: OrderKey{Counter: 1, Tag: "t2"}}

	for seed := int64(0); seed < 50; seed++ {
		s := applyIn(shuffled([]Change{add1, remove, add2}, seed))
		ids := s.IDs()
		if len(ids) != 1 || ids[0] != "song" {
			t.Fatalf("seed=%d: add-wins failed, got %v", seed, ids)
		}
	}
}

// TestZeroChangeIsAbsorbedDeterministically: a zero-value Change (Op defaults to
// OpAdd, empty tag, empty item id) is a degenerate add. Whatever it does, it must
// do it order-independently — an attacker cannot use a zero change to make two
// replicas disagree.
func TestZeroChangeIsAbsorbedDeterministically(t *testing.T) {
	t.Parallel()
	zero := Change{}
	real := Change{Op: OpAdd, ItemID: "x", Tag: "t", Order: OrderKey{Counter: 1, Tag: "t"}}

	a := applyIn([]Change{zero, real})
	b := applyIn([]Change{real, zero})
	if a.Encode() != b.Encode() {
		t.Fatalf("a zero change made the state order-dependent:\n%s\nvs\n%s", a.Encode(), b.Encode())
	}
}

// TestCollidingTagsConverge: two adds under the SAME tag with different payloads
// — which a globally-unique UUIDv7 tag never produces, but an unsigned Change from
// a malicious device could — converge to the SAME record on every replica,
// regardless of arrival order. Without the lattice join this was last-write-wins
// by order and the two replicas DIVERGED; this pins the fix. Convergence (§43) is
// the property a shared space (§47) cannot let a co-member break.
func TestCollidingTagsConverge(t *testing.T) {
	t.Parallel()
	addX := Change{Op: OpAdd, ItemID: "X", Tag: "same", Order: OrderKey{Counter: 1, Tag: "same"}}
	addY := Change{Op: OpAdd, ItemID: "Y", Tag: "same", Order: OrderKey{Counter: 2, Tag: "same"}}

	xy := applyIn([]Change{addX, addY})
	yx := applyIn([]Change{addY, addX})
	if xy.Encode() != yx.Encode() {
		t.Fatalf("colliding tags diverged by order:\n%s\nvs\n%s", xy.Encode(), yx.Encode())
	}
	// Merge must resolve the collision identically to Apply.
	if Merge(applyIn([]Change{addX}), applyIn([]Change{addY})).Encode() != xy.Encode() {
		t.Fatal("Merge resolved a colliding tag differently from Apply")
	}
	// The deterministic winner is the lesser by (itemID, order): "X" < "Y".
	if ids := xy.IDs(); len(ids) != 1 || ids[0] != "X" {
		t.Fatalf("colliding tags did not settle on the deterministic record: %v", ids)
	}

	// The other branch of the join: SAME tag AND same item id, different order.
	// The tie-break must be the order key, deterministically, not arrival order.
	lo := Change{Op: OpAdd, ItemID: "Z", Tag: "dup", Order: OrderKey{Counter: 1, Tag: "dup"}}
	hi := Change{Op: OpAdd, ItemID: "Z", Tag: "dup", Order: OrderKey{Counter: 9, Tag: "dup"}}
	if applyIn([]Change{lo, hi}).Encode() != applyIn([]Change{hi, lo}).Encode() {
		t.Fatal("same-tag same-item different-order diverged by arrival order")
	}
	// The lesser order (counter 1) wins on every replica.
	if got := applyIn([]Change{hi, lo}).Items(); len(got) != 1 || got[0].Order.Counter != 1 {
		t.Fatalf("the order tie-break did not settle deterministically: %+v", got)
	}
}

// TestPoisonedClockDoesNotWrap: a change carrying Counter=MaxUint64 poisons the
// Lamport clock, but the next local Add must NOT wrap to 0 (which would sort new
// inserts before everything). Saturating keeps them at MaxUint64, sorting last.
func TestPoisonedClockDoesNotWrap(t *testing.T) {
	t.Parallel()
	s := New()
	s.Apply(Change{Op: OpAdd, ItemID: "poison", Tag: "p", Order: OrderKey{Counter: math.MaxUint64, Tag: "p"}})

	c := s.Add("next")
	if c.Order.Counter == 0 {
		t.Fatal("counter wrapped to 0 after MaxUint64: a poisoned clock reorders every future insert")
	}
	if c.Order.Counter != math.MaxUint64 {
		t.Fatalf("saturated counter = %d, want MaxUint64", c.Order.Counter)
	}
	// Both items are present; they share the MaxUint64 counter and settle by their
	// tie-break tags — a bounded, deterministic order, not the front-jumping chaos
	// a wrap-to-0 would cause. (The exact position depends on Add's minted tag, so
	// this asserts presence and the shared saturated counter, not a fixed slot.)
	if len(s.IDs()) != 2 {
		t.Fatalf("a poisoned-clock insert lost an item: %v", s.IDs())
	}
}

// TestMergeIgnoresNilStates: Merge tolerates nil inputs (a dropped replica) rather
// than panicking, and nil contributes nothing.
func TestMergeIgnoresNilStates(t *testing.T) {
	t.Parallel()
	s := applyIn([]Change{{Op: OpAdd, ItemID: "x", Tag: "t", Order: OrderKey{Counter: 1, Tag: "t"}}})
	got := Merge(nil, s, nil, nil)
	if got.Encode() != s.Encode() {
		t.Fatal("nil states changed the merge result")
	}
}
