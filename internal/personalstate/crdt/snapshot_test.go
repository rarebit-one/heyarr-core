package crdt

import "testing"

// A snapshot round-trips: FromSnapshot(s.Snapshot()) is an equal state, byte for
// byte under Encode.
func TestSnapshotRoundTrips(t *testing.T) {
	s := New()
	s.Apply(s.Add("a"), s.Add("b"))
	rem := s.Remove("a")
	s.Apply(rem)
	s.Apply(s.Add("c"))

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if s.Encode() != back.Encode() {
		t.Fatalf("snapshot did not round-trip:\n%s\n---\n%s", s.Encode(), back.Encode())
	}
}

// A snapshot plus the tail of changes after it reaches the SAME state as
// replaying the whole log — the property that lets a fresh device sync bounded.
func TestSnapshotPlusTailEqualsFullReplay(t *testing.T) {
	// Full history.
	full := New()
	c1 := full.Add("x")
	c2 := full.Add("y")
	// Snapshot taken here (after c1, c2).
	snapAt := New()
	snapAt.Apply(c1, c2)
	snap, err := snapAt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Tail: two more changes.
	c3 := full.Add("z")
	c4 := full.Remove("x")
	full.Apply(c3, c4)

	// A fresh device: snapshot + tail.
	fresh, err := FromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Apply(c3, c4)

	if full.Encode() != fresh.Encode() {
		t.Fatalf("snapshot+tail != full replay:\nfull: %v\nfresh: %v", full.IDs(), fresh.IDs())
	}
}

// Two converged states snapshot to byte-identical output — the property that
// makes a snapshot content-addressable.
func TestConvergedStatesSnapshotIdentically(t *testing.T) {
	a := New()
	ca := a.Add("one")
	cb := a.Add("two")
	// b applies the same changes in the OTHER order.
	b := New()
	b.Apply(cb, ca)
	a.Apply() // no-op to mirror

	sa, _ := a.Snapshot()
	sb, _ := b.Snapshot()
	if string(sa) != string(sb) {
		t.Fatalf("converged states snapshot differently:\n%s\n%s", sa, sb)
	}
}
