package crdt

// history_snapshot.go serialises a materialised [PlayLog] and reconstructs it
// (§44): the whole grow-only event set at a causal point — every event and the
// Lamport counter — so a fresh or long-offline device reaches the converged
// history from a snapshot plus the tail of plays after it. The encoding is
// deterministic (events sorted by tag), so two converged logs snapshot to
// byte-identical output, safe to content-address.
//
// A play history grows without bound by design (§46 leaves compaction to the
// snapshot layer, #325/#330): a snapshot is the point at which a device can drop
// the change tail it folded, exactly as the playlist snapshot does.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type playSnapshotEvent struct {
	Tag    PlayTag `json:"tag"`
	ItemID string  `json:"item"`
	At     uint64  `json:"at"`
}

type playLogSnapshot struct {
	Events  []playSnapshotEvent `json:"events"`
	Counter uint64              `json:"counter"`
}

// Snapshot serialises the entire log deterministically. Two converged logs
// produce identical bytes, so the result is safe to content-address.
func (l *PlayLog) Snapshot() ([]byte, error) {
	snap := playLogSnapshot{
		Events:  make([]playSnapshotEvent, 0, len(l.events)),
		Counter: l.counter,
	}
	for tag, rec := range l.events {
		snap.Events = append(snap.Events, playSnapshotEvent{Tag: tag, ItemID: rec.itemID, At: rec.at})
	}
	sort.Slice(snap.Events, func(i, j int) bool { return snap.Events[i].Tag < snap.Events[j].Tag })
	return json.Marshal(snap)
}

// PlayLogFromSnapshot reconstructs a log from Snapshot output. Applying the plays
// AFTER the snapshot's causal point yields the same log as replaying the whole
// history, because the G-Set is a semilattice and a snapshot is a partial fold.
func PlayLogFromSnapshot(b []byte) (*PlayLog, error) {
	var snap playLogSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding a play-history snapshot: %w", err)
	}
	l := NewPlayLog()
	for _, e := range snap.Events {
		l.events[e.Tag] = playRecord{itemID: e.ItemID, at: e.At}
	}
	l.counter = snap.Counter
	return l, nil
}
