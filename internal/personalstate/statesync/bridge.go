// Package statesync bridges the client-side CRDT and the opaque encrypted changes
// the sync protocol moves, using the device's space key. It is the device-side
// glue of §42, §43: a CRDT operation becomes an encrypted, content-addressed
// change to ship; an encrypted change fetched from a peer becomes a CRDT
// operation to merge. The peer in between only ever sees ciphertext — the whole
// point of the plane (Invariant 6).
//
// It composes three packages and adds no new mechanism: crdt (the merge logic),
// client (the device's space key and its encrypt/decrypt), and protocol (the
// opaque wire change and its content-addressed id).
package statesync

import (
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// EncodeChange encrypts ANY CRDT change type under an open space's key and wraps
// it as a content-addressed [protocol.EncryptedChange], ready to push to a peer.
// It is generic over the change type so every personal-state CRDT — the playlist
// [crdt.Change], the starred [crdt.StarChange], the reading-position
// [crdt.PositionChange] and the play-history [crdt.PlayChange] (issue #386) —
// shares one bridge and one opacity guarantee, rather than each growing its own
// encrypt path. parents are the space's current causal heads (from a prior sync).
// The space must be open on m, or Encrypt refuses.
//
// The wire form carries no type tag: which CRDT a change belongs to is a property
// of the space it lives in (a space holds one CRDT), decided by the caller that
// opens the space and merges into the matching [crdt] materialiser — not
// something a peer holding opaque ciphertext could or should learn.
func EncodeChange[T any](m *client.Manager, spaceID string, parents []string, ch T) (protocol.EncryptedChange, error) {
	raw, err := json.Marshal(ch)
	if err != nil {
		return protocol.EncryptedChange{}, fmt.Errorf("statesync: encoding change: %w", err)
	}
	ct, err := m.Encrypt(spaceID, raw)
	if err != nil {
		return protocol.EncryptedChange{}, err
	}
	return protocol.NewChange(spaceID, parents, ct)
}

// DecodeChange decrypts an encrypted change fetched from a peer, under the open
// space's key, back into a CRDT change of type T to merge. It first VALIDATES the
// change's id against its bytes (Invariant 1, ADR-0005) — a client trusts a
// claimed id no more than a peer does — then decrypts. A device that does not
// hold the space key cannot decode it (Decrypt refuses). The caller chooses T to
// match the CRDT the space holds.
func DecodeChange[T any](m *client.Manager, ec protocol.EncryptedChange) (T, error) {
	var ch T
	if err := ec.Validate(); err != nil {
		return ch, fmt.Errorf("statesync: refusing a change: %w", err)
	}
	raw, err := m.Decrypt(ec.SpaceID, ec.Ciphertext)
	if err != nil {
		return ch, err
	}
	if err := json.Unmarshal(raw, &ch); err != nil {
		return ch, fmt.Errorf("statesync: decoding change: %w", err)
	}
	return ch, nil
}

// DecodeAllChanges decodes a batch of encrypted changes into CRDT changes of type
// T, ready to apply to the matching materialiser in one merge. A change that
// fails to validate or decrypt fails the batch, because a hole would silently
// diverge the merge — better to surface it and re-sync than to apply a partial
// set as if complete.
func DecodeAllChanges[T any](m *client.Manager, changes []protocol.EncryptedChange) ([]T, error) {
	out := make([]T, 0, len(changes))
	for _, ec := range changes {
		ch, err := DecodeChange[T](m, ec)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// Encode is the playlist-change bridge, kept as the original non-generic entry
// point every current caller uses. It is [EncodeChange] pinned to [crdt.Change].
func Encode(m *client.Manager, spaceID string, parents []string, ch crdt.Change) (protocol.EncryptedChange, error) {
	return EncodeChange(m, spaceID, parents, ch)
}

// Decode is the playlist-change bridge, kept for existing callers. It is
// [DecodeChange] pinned to [crdt.Change].
func Decode(m *client.Manager, ec protocol.EncryptedChange) (crdt.Change, error) {
	return DecodeChange[crdt.Change](m, ec)
}

// DecodeAll is the playlist-change batch bridge, kept for existing callers. It is
// [DecodeAllChanges] pinned to [crdt.Change].
func DecodeAll(m *client.Manager, changes []protocol.EncryptedChange) ([]crdt.Change, error) {
	return DecodeAllChanges[crdt.Change](m, changes)
}
