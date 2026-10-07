package crdt

// strict.go recognises a playlist change or snapshot by its exact wire shape.
//
// The encrypted wire form carries no type tag: a space holds one CRDT, and which
// one is the caller's knowledge (statesync.EncodeChange). A lenient json.Unmarshal
// into the playlist [Change] does NOT fail on another CRDT's change — a drive
// change's "op" lands case-insensitively on Change.Op and every other field is
// dropped — so a caller that must not mistake one CRDT for another (rotation,
// which re-snapshots and compacts the log, #698) decodes strictly instead. Every
// other change type carries a field the playlist one lacks (drive: path/at/writer;
// starred and play history: At; reading position: PubID), and every other
// snapshot a top-level key the playlist one lacks, so a strict decode tells them
// apart without a tag on the wire.

import (
	"bytes"
	"encoding/json"
)

// IsPlaylistChange reports whether b decodes as a playlist [Change] with no
// field left over.
func IsPlaylistChange(b []byte) bool {
	var ch Change
	return decodeStrict(b, &ch)
}

// IsPlaylistSnapshot reports whether b decodes as a playlist [State] snapshot
// with no field left over.
func IsPlaylistSnapshot(b []byte) bool {
	var snap stateSnapshot
	return decodeStrict(b, &snap)
}

func decodeStrict(b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	// Exactly one JSON value: trailing bytes are not a playlist record either.
	return !dec.More()
}
