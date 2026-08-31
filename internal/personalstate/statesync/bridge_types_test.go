package statesync_test

import (
	"bytes"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// These tests prove the generic bridge (EncodeChange/DecodeChange/
// DecodeAllChanges) carries every personal-state CRDT change type added in #386 —
// starred, reading-position and play-history — with the same opacity guarantee
// the playlist bridge has: two devices converge through the peer, and the peer
// holding the ciphertext learns nothing (Invariant 6, §72).

// TestStarredConvergesThroughEncryptedChanges: device A stars items, ships them
// as opaque encrypted changes; device B — holding only its wrapped space key —
// decrypts and merges, and the two starred sets converge.
func TestStarredConvergesThroughEncryptedChanges(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	_, rb, ub := device(t)

	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindShared, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}

	stateA := crdt.NewStarSet()
	var wire []protocol.EncryptedChange
	for _, item := range []string{"alpha", "bravo", "charlie"} {
		ec, err := statesync.EncodeChange(mgrA, sp.ID, nil, stateA.Star(item))
		if err != nil {
			t.Fatalf("EncodeChange %s: %v", item, err)
		}
		wire = append(wire, ec)
	}

	mgrB := client.New()
	if err := mgrB.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	decoded, err := statesync.DecodeAllChanges[crdt.StarChange](mgrB, wire)
	if err != nil {
		t.Fatalf("B.DecodeAllChanges: %v", err)
	}
	stateB := crdt.NewStarSet()
	stateB.Apply(decoded...)

	if !equalStrings(stateA.StarredIDs(), stateB.StarredIDs()) {
		t.Fatalf("starred did not converge: A=%v B=%v", stateA.StarredIDs(), stateB.StarredIDs())
	}
}

// TestReadingPositionConvergesThroughEncryptedChanges: the reading-position LWW
// register round-trips through the bridge and converges across two devices.
func TestReadingPositionConvergesThroughEncryptedChanges(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	_, rb, ub := device(t)

	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindShared, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}

	stateA := crdt.NewReadingPositions()
	var wire []protocol.EncryptedChange
	for _, w := range []struct{ pub, pos string }{{"book-1", "p10"}, {"book-2", "p3"}, {"book-1", "p22"}} {
		ec, err := statesync.EncodeChange(mgrA, sp.ID, nil, stateA.Set(w.pub, w.pos))
		if err != nil {
			t.Fatalf("EncodeChange %s: %v", w.pub, err)
		}
		wire = append(wire, ec)
	}

	mgrB := client.New()
	if err := mgrB.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	decoded, err := statesync.DecodeAllChanges[crdt.PositionChange](mgrB, wire)
	if err != nil {
		t.Fatalf("B.DecodeAllChanges: %v", err)
	}
	stateB := crdt.NewReadingPositions()
	stateB.Apply(decoded...)

	if stateA.Encode() != stateB.Encode() {
		t.Fatalf("reading positions did not converge:\n%s\n---\n%s", stateA.Encode(), stateB.Encode())
	}
	if got, _ := stateB.Position("book-1"); got != "p22" {
		t.Fatalf("latest position did not survive the round trip: got %q", got)
	}
}

// TestPlayHistoryConvergesThroughEncryptedChanges: the play-history G-Set round-
// trips through the bridge; counts sum across the two devices' events.
func TestPlayHistoryConvergesThroughEncryptedChanges(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	_, rb, ub := device(t)

	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindShared, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}

	stateA := crdt.NewPlayLog()
	var wire []protocol.EncryptedChange
	for _, item := range []string{"song", "song", "other"} {
		ec, err := statesync.EncodeChange(mgrA, sp.ID, nil, stateA.Record(item))
		if err != nil {
			t.Fatalf("EncodeChange %s: %v", item, err)
		}
		wire = append(wire, ec)
	}

	mgrB := client.New()
	if err := mgrB.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	decoded, err := statesync.DecodeAllChanges[crdt.PlayChange](mgrB, wire)
	if err != nil {
		t.Fatalf("B.DecodeAllChanges: %v", err)
	}
	stateB := crdt.NewPlayLog()
	stateB.Apply(decoded...)

	if stateA.Encode() != stateB.Encode() {
		t.Fatalf("play history did not converge:\n%s\n---\n%s", stateA.Encode(), stateB.Encode())
	}
	if got := stateB.Count("song"); got != 2 {
		t.Fatalf("play count did not survive the round trip: got %d, want 2", got)
	}
}

// TestNewChangeTypesAreOpaqueToThePeer: none of the new change types leaks its
// plaintext (item id / position) into the wire ciphertext a peer holds.
func TestNewChangeTypesAreOpaqueToThePeer(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}

	star, err := statesync.EncodeChange(m, sp.ID, nil, crdt.NewStarSet().Star("secret-star-id"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(star.Ciphertext, []byte("secret-star-id")) {
		t.Fatal("the encrypted star leaks the item id in the clear")
	}

	pos, err := statesync.EncodeChange(m, sp.ID, nil, crdt.NewReadingPositions().Set("secret-book", "secret-locator"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pos.Ciphertext, []byte("secret-book")) || bytes.Contains(pos.Ciphertext, []byte("secret-locator")) {
		t.Fatal("the encrypted position leaks the publication or locator in the clear")
	}

	play, err := statesync.EncodeChange(m, sp.ID, nil, crdt.NewPlayLog().Record("secret-track"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(play.Ciphertext, []byte("secret-track")) {
		t.Fatal("the encrypted play leaks the track id in the clear")
	}
}

// TestGenericDecodeRejectsForgedChange: the generic decode path validates the
// change id against its bytes for a new type too, refusing a tampered change.
func TestGenericDecodeRejectsForgedChange(t *testing.T) {
	t.Parallel()
	_, ra, _ := device(t)
	m := client.New()
	sp, _, err := m.Create(spaces.KindPersonal, testNow, []client.Recipient{ra})
	if err != nil {
		t.Fatal(err)
	}
	ec, err := statesync.EncodeChange(m, sp.ID, nil, crdt.NewStarSet().Star("x"))
	if err != nil {
		t.Fatal(err)
	}
	ec.Ciphertext = append([]byte("tamper"), ec.Ciphertext...)
	if _, err := statesync.DecodeChange[crdt.StarChange](m, ec); err == nil {
		t.Fatal("a forged star change decoded")
	}
}
