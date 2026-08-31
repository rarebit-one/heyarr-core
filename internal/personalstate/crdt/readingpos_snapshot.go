package crdt

// readingpos_snapshot.go serialises a materialised [ReadingPositions] map and
// reconstructs it (§44): the whole per-publication register at a causal point —
// every position, its order key, and the Lamport counter — so a fresh or
// long-offline device reaches the converged map from a snapshot plus the tail of
// writes after it. The encoding is deterministic (sorted by publication id), so
// two converged maps snapshot to byte-identical output, safe to content-address.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type posSnapshotEntry struct {
	PubID    string `json:"pub"`
	Position string `json:"position"`
	At       uint64 `json:"at"`
	Writer   PosTag `json:"writer"`
}

type readingSnapshot struct {
	Positions []posSnapshotEntry `json:"positions"`
	Counter   uint64             `json:"counter"`
}

// Snapshot serialises the entire map deterministically. Two converged maps
// produce identical bytes, so the result is safe to content-address.
func (r *ReadingPositions) Snapshot() ([]byte, error) {
	snap := readingSnapshot{
		Positions: make([]posSnapshotEntry, 0, len(r.positions)),
		Counter:   r.counter,
	}
	for pub, rec := range r.positions {
		snap.Positions = append(snap.Positions, posSnapshotEntry{
			PubID: pub, Position: rec.position, At: rec.key.At, Writer: rec.key.Writer,
		})
	}
	sort.Slice(snap.Positions, func(i, j int) bool { return snap.Positions[i].PubID < snap.Positions[j].PubID })
	return json.Marshal(snap)
}

// ReadingPositionsFromSnapshot reconstructs a map from Snapshot output. Applying
// the writes AFTER the snapshot's causal point yields the same map as replaying
// the whole log, because the register is a semilattice and a snapshot is a
// partial fold.
func ReadingPositionsFromSnapshot(b []byte) (*ReadingPositions, error) {
	var snap readingSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding a reading-position snapshot: %w", err)
	}
	r := NewReadingPositions()
	for _, p := range snap.Positions {
		r.positions[p.PubID] = posRecord{position: p.Position, key: posKey{At: p.At, Writer: p.Writer}}
	}
	r.counter = snap.Counter
	return r, nil
}
