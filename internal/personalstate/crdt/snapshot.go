package crdt

// snapshot.go serialises a materialised CRDT state and reconstructs it (§44). A
// snapshot is the whole OR-Set at a causal point — every add-tag, every
// tombstone, and the Lamport counter — so a fresh or long-offline device can
// reach the converged state from a SNAPSHOT plus the tail of changes after it,
// rather than replaying the entire log (the bounded sync a phone needs, #330).
//
// The encoding is deterministic (every set sorted), so two states that have
// converged snapshot to BYTE-IDENTICAL output. That is what lets a snapshot be
// content-addressed and lets two peers agree one was taken at the same point.
// Unlike [State.Encode] — a human-readable debug aid — this is a round-trip:
// FromSnapshot(s.Snapshot()) reconstructs an equal state.

import (
	"encoding/json"
	"fmt"
	"sort"
)

// snapshotAdd is one live-or-dead add-tag in a serialised state: the tag, the
// item it introduced, and its total-order key (counter + tie-break tag).
type snapshotAdd struct {
	Tag      Tag    `json:"tag"`
	ItemID   string `json:"item"`
	Counter  uint64 `json:"counter"`
	OrderTag Tag    `json:"order_tag"`
}

// stateSnapshot is the wire form of a whole [State]. It is what gets encrypted
// under the space key and stored as an opaque snapshot — the peer never sees this
// shape, only its ciphertext.
type stateSnapshot struct {
	Adds       []snapshotAdd `json:"adds"`
	Tombstones []Tag         `json:"tombstones"`
	Counter    uint64        `json:"counter"`
}

// Snapshot serialises the entire state deterministically. Two converged states
// produce identical bytes, so the result is safe to content-address.
func (s *State) Snapshot() ([]byte, error) {
	snap := stateSnapshot{
		Adds:       make([]snapshotAdd, 0, len(s.adds)),
		Tombstones: make([]Tag, 0, len(s.tombstones)),
		Counter:    s.counter,
	}
	for tag, rec := range s.adds {
		snap.Adds = append(snap.Adds, snapshotAdd{
			Tag:      tag,
			ItemID:   rec.itemID,
			Counter:  rec.order.Counter,
			OrderTag: rec.order.Tag,
		})
	}
	sort.Slice(snap.Adds, func(i, j int) bool { return snap.Adds[i].Tag < snap.Adds[j].Tag })
	for tag := range s.tombstones {
		snap.Tombstones = append(snap.Tombstones, tag)
	}
	sort.Slice(snap.Tombstones, func(i, j int) bool { return snap.Tombstones[i] < snap.Tombstones[j] })
	return json.Marshal(snap)
}

// FromSnapshot reconstructs a state from Snapshot output. Applying the changes
// AFTER the snapshot's causal point to the reconstructed state yields the same
// state as replaying the whole log — because the OR-Set is a semilattice and a
// snapshot is just a partial fold of it.
func FromSnapshot(b []byte) (*State, error) {
	var snap stateSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding a snapshot: %w", err)
	}
	s := New()
	for _, a := range snap.Adds {
		s.adds[a.Tag] = addRecord{itemID: a.ItemID, order: OrderKey{Counter: a.Counter, Tag: a.OrderTag}}
	}
	for _, tag := range snap.Tombstones {
		s.tombstones[tag] = struct{}{}
	}
	s.counter = snap.Counter
	return s, nil
}
