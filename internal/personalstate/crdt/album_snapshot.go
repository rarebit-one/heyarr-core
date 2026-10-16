package crdt

// album_snapshot.go serialises a materialised [Albums] set and reconstructs it
// (§44): every album's accumulated write set — lifecycle keys, registers, item
// adds, removals and moves — plus the Lamport counter, so a fresh or long-offline
// device reaches the converged albums from a snapshot plus the tail of writes
// after it. The encoding is deterministic (albums and every list sorted), so two
// converged sets snapshot to byte-identical output, safe to content-address.
//
// It rides the generic snapshot bridge (statesync.EncodeSnapshotOf /
// DecodeSnapshotWith) exactly as the drive does; the peer stores the ciphertext
// and never sees this shape (Invariant 6). The top-level key is `albums`, which no
// other kind's snapshot carries, so a strict decode tells it apart on the wire.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type albumRegSnapshot struct {
	Value  string `json:"value"`
	At     uint64 `json:"at"`
	Writer PosTag `json:"writer"`
}

type albumItemSnapshot struct {
	At     uint64        `json:"at"`
	Writer PosTag        `json:"writer"`
	Pos    uint64        `json:"pos"`
	Item   PhotoManifest `json:"item"`
}

type albumMoveSnapshot struct {
	ItemAt uint64 `json:"item_at"`
	ItemBy PosTag `json:"item_by"`
	Pos    uint64 `json:"pos"`
	At     uint64 `json:"at"`
	Writer PosTag `json:"writer"`
}

type albumRecordSnapshot struct {
	AlbumID   string              `json:"album_id"`
	CreatedAt uint64              `json:"created_at,omitempty"`
	CreatedBy PosTag              `json:"created_by,omitempty"`
	DelAt     uint64              `json:"del_at,omitempty"`
	DelBy     PosTag              `json:"del_by,omitempty"`
	Title     *albumRegSnapshot   `json:"title,omitempty"`
	Caption   *albumRegSnapshot   `json:"caption,omitempty"`
	Cover     *albumRegSnapshot   `json:"cover,omitempty"`
	Items     []albumItemSnapshot `json:"items"`
	Removed   []WriteKey          `json:"removed"`
	Moves     []albumMoveSnapshot `json:"moves"`
}

type albumsSnapshot struct {
	Albums  []albumRecordSnapshot `json:"albums"`
	Counter uint64                `json:"counter"`
}

func regSnapshot(r albumReg) *albumRegSnapshot {
	if !r.set {
		return nil
	}
	return &albumRegSnapshot{Value: r.value, At: r.key.At, Writer: r.key.Writer}
}

func regFromSnapshot(s *albumRegSnapshot) albumReg {
	if s == nil {
		return albumReg{}
	}
	return albumReg{set: true, value: s.Value, key: posKey{At: s.At, Writer: s.Writer}}
}

func lessKey(a, b posKey) bool { return b.greater(a) }

// Snapshot serialises the entire set deterministically. Two converged sets
// produce identical bytes, so the result is safe to content-address.
func (a *Albums) Snapshot() ([]byte, error) {
	snap := albumsSnapshot{Albums: make([]albumRecordSnapshot, 0, len(a.albums)), Counter: a.counter}
	for id, rec := range a.albums {
		rs := albumRecordSnapshot{
			AlbumID:   id,
			CreatedAt: rec.createKey.At, CreatedBy: rec.createKey.Writer,
			DelAt: rec.delKey.At, DelBy: rec.delKey.Writer,
			Title:   regSnapshot(rec.title),
			Caption: regSnapshot(rec.caption),
			Cover:   regSnapshot(rec.cover),
			Items:   make([]albumItemSnapshot, 0, len(rec.items)),
			Removed: make([]WriteKey, 0, len(rec.removed)),
			Moves:   make([]albumMoveSnapshot, 0, len(rec.moves)),
		}
		for k, it := range rec.items {
			rs.Items = append(rs.Items, albumItemSnapshot{At: k.At, Writer: k.Writer, Pos: it.pos, Item: it.photo})
		}
		sort.Slice(rs.Items, func(i, j int) bool {
			return lessKey(posKey{At: rs.Items[i].At, Writer: rs.Items[i].Writer}, posKey{At: rs.Items[j].At, Writer: rs.Items[j].Writer})
		})
		for k := range rec.removed {
			rs.Removed = append(rs.Removed, keyOf(k))
		}
		sort.Slice(rs.Removed, func(i, j int) bool { return lessKey(rs.Removed[i].key(), rs.Removed[j].key()) })
		for k, mv := range rec.moves {
			rs.Moves = append(rs.Moves, albumMoveSnapshot{ItemAt: k.At, ItemBy: k.Writer, Pos: mv.pos, At: mv.key.At, Writer: mv.key.Writer})
		}
		sort.Slice(rs.Moves, func(i, j int) bool {
			return lessKey(posKey{At: rs.Moves[i].ItemAt, Writer: rs.Moves[i].ItemBy}, posKey{At: rs.Moves[j].ItemAt, Writer: rs.Moves[j].ItemBy})
		})
		snap.Albums = append(snap.Albums, rs)
	}
	sort.Slice(snap.Albums, func(i, j int) bool { return snap.Albums[i].AlbumID < snap.Albums[j].AlbumID })
	return json.Marshal(snap)
}

// AlbumsFromSnapshot reconstructs a set from Snapshot output. Applying the writes
// AFTER the snapshot's causal point yields the same set as replaying the whole
// log, because the map is a semilattice and a snapshot is a partial fold.
func AlbumsFromSnapshot(b []byte) (*Albums, error) {
	var snap albumsSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("crdt: decoding an album snapshot: %w", err)
	}
	a := NewAlbums()
	for _, rs := range snap.Albums {
		rec := newAlbumRecord()
		rec.createKey = posKey{At: rs.CreatedAt, Writer: rs.CreatedBy}
		rec.delKey = posKey{At: rs.DelAt, Writer: rs.DelBy}
		rec.title = regFromSnapshot(rs.Title)
		rec.caption = regFromSnapshot(rs.Caption)
		rec.cover = regFromSnapshot(rs.Cover)
		for _, it := range rs.Items {
			rec.items[posKey{At: it.At, Writer: it.Writer}] = albumItem{photo: it.Item, pos: it.Pos}
		}
		for _, k := range rs.Removed {
			rec.removed[k.key()] = struct{}{}
		}
		for _, mv := range rs.Moves {
			rec.moves[posKey{At: mv.ItemAt, Writer: mv.ItemBy}] = albumMove{pos: mv.Pos, key: posKey{At: mv.At, Writer: mv.Writer}}
		}
		a.albums[rs.AlbumID] = rec
	}
	a.counter = snap.Counter
	return a, nil
}
