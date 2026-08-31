package crdt

// starred_snapshot.go serialises a materialised [StarSet] and reconstructs it
// (§44), mirroring the playlist snapshot: the whole OR-Set at a causal point —
// every star-tag, every tombstone, and the Lamport counter — so a fresh or
// long-offline device reaches the converged set from a snapshot plus the tail of
// changes after it, rather than replaying the entire log. The encoding is
// deterministic (every set sorted), so two converged sets snapshot to
// byte-identical output and the result is safe to content-address.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type starSnapshotAdd struct {
	Tag    StarTag `json:"tag"`
	ItemID string  `json:"item"`
	At     uint64  `json:"at"`
}

type starSetSnapshot struct {
	Stars      []starSnapshotAdd `json:"stars"`
	Tombstones []StarTag         `json:"tombstones"`
	Counter    uint64            `json:"counter"`
}

// Snapshot serialises the entire set deterministically. Two converged sets
// produce identical bytes, so the result is safe to content-address.
func (s *StarSet) Snapshot() ([]byte, error) {
	snap := starSetSnapshot{
		Stars:      make([]starSnapshotAdd, 0, len(s.adds)),
		Tombstones: make([]StarTag, 0, len(s.tombstones)),
		Counter:    s.counter,
	}
	for tag, rec := range s.adds {
		snap.Stars = append(snap.Stars, starSnapshotAdd{Tag: tag, ItemID: rec.itemID, At: rec.at})
	}
	sort.Slice(snap.Stars, func(i, j int) bool { return snap.Stars[i].Tag < snap.Stars[j].Tag })
	for tag := range s.tombstones {
		snap.Tombstones = append(snap.Tombstones, tag)
	}
	sort.Slice(snap.Tombstones, func(i, j int) bool { return snap.Tombstones[i] < snap.Tombstones[j] })
	return json.Marshal(snap)
}

// StarSetFromSnapshot reconstructs a set from Snapshot output. Applying the
// changes AFTER the snapshot's causal point yields the same set as replaying the
// whole log, because the OR-Set is a semilattice and a snapshot is a partial fold.
func StarSetFromSnapshot(b []byte) (*StarSet, error) {
	var snap starSetSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding a starred snapshot: %w", err)
	}
	s := NewStarSet()
	for _, a := range snap.Stars {
		s.adds[a.Tag] = starRecord{itemID: a.ItemID, at: a.At}
	}
	for _, tag := range snap.Tombstones {
		s.tombstones[tag] = struct{}{}
	}
	s.counter = snap.Counter
	return s, nil
}
