package crdt

// drive_snapshot.go serialises a materialised [Drive] and reconstructs it (§44):
// the whole path-keyed map at a causal point — every path's accumulated write set
// and its tombstone, plus the Lamport counter — so a fresh or long-offline device
// reaches the converged drive from a snapshot plus the tail of writes after it.
// The encoding is deterministic (paths and writes sorted), so two converged drives
// snapshot to byte-identical output, safe to content-address (ADR-0095, ADR-0021).
//
// The drive snapshot is bridged for replication (W2/#538): the statesync snapshot
// bridge was generalised behind a { Snapshot() ([]byte, error) } interface, and
// statesync.EncodeDriveSnapshot / DecodeDriveSnapshot pin it to *Drive — the drive
// counterparts of the playlist EncodeSnapshot / DecodeSnapshot. Drive CHANGES
// already ride the generic change bridge (statesync.EncodeChange[DriveChange]).

import (
	"encoding/json"
	"fmt"
	"sort"
)

type driveValueSnapshot struct {
	Blob     string `json:"blob"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
	At       uint64 `json:"at"`
	Writer   PosTag `json:"writer"`
	BaseAt   uint64 `json:"baseAt,omitempty"`
	BaseWrit PosTag `json:"baseBy,omitempty"`
}

type driveEntrySnapshot struct {
	Path   string               `json:"path"`
	Writes []driveValueSnapshot `json:"writes"`
	DelAt  uint64               `json:"delAt,omitempty"`
	DelBy  PosTag               `json:"delBy,omitempty"`
}

type driveSnapshot struct {
	Entries []driveEntrySnapshot `json:"entries"`
	Counter uint64               `json:"counter"`
}

// Snapshot serialises the entire drive deterministically. Two converged drives
// produce identical bytes, so the result is safe to content-address.
func (d *Drive) Snapshot() ([]byte, error) {
	snap := driveSnapshot{
		Entries: make([]driveEntrySnapshot, 0, len(d.entries)),
		Counter: d.counter,
	}
	for p, rec := range d.entries {
		writes := make([]driveValueSnapshot, 0, len(rec.values))
		for _, v := range rec.values {
			writes = append(writes, driveValueSnapshot{
				Blob: v.Blob, Size: v.Size, MTime: v.MTime,
				At: v.key.At, Writer: v.key.Writer, BaseAt: v.base.At, BaseWrit: v.base.Writer,
			})
		}
		sort.Slice(writes, func(i, j int) bool {
			if writes[i].At != writes[j].At {
				return writes[i].At < writes[j].At
			}
			return writes[i].Writer < writes[j].Writer
		})
		snap.Entries = append(snap.Entries, driveEntrySnapshot{
			Path: p, Writes: writes, DelAt: rec.delKey.At, DelBy: rec.delKey.Writer,
		})
	}
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	return json.Marshal(snap)
}

// DriveFromSnapshot reconstructs a drive from Snapshot output. Applying the writes
// AFTER the snapshot's causal point yields the same drive as replaying the whole
// log, because the map is a semilattice and a snapshot is a partial fold.
func DriveFromSnapshot(b []byte) (*Drive, error) {
	var snap driveSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding a drive snapshot: %w", err)
	}
	d := NewDrive()
	for _, e := range snap.Entries {
		rec := driveRecord{values: make(map[posKey]driveValue, len(e.Writes)), delKey: posKey{At: e.DelAt, Writer: e.DelBy}}
		for _, w := range e.Writes {
			k := posKey{At: w.At, Writer: w.Writer}
			rec.values[k] = driveValue{Blob: w.Blob, Size: w.Size, MTime: w.MTime, key: k, base: posKey{At: w.BaseAt, Writer: w.BaseWrit}}
		}
		d.entries[e.Path] = rec
	}
	d.counter = snap.Counter
	return d, nil
}
