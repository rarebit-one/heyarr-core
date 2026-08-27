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

// Encode encrypts a CRDT change under an open space's key and wraps it as a
// content-addressed [protocol.EncryptedChange], ready to push to a peer. parents
// are the space's current causal heads (from a prior sync). The space must be
// open on m, or Encrypt refuses.
func Encode(m *client.Manager, spaceID string, parents []string, ch crdt.Change) (protocol.EncryptedChange, error) {
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

// Decode decrypts an encrypted change fetched from a peer, under the open space's
// key, back into a CRDT change to merge. It first VALIDATES the change's id
// against its bytes (Invariant 1, ADR-0005) — a client trusts a claimed id no
// more than a peer does — then decrypts. A device that does not hold the space
// key cannot decode it (Decrypt refuses).
func Decode(m *client.Manager, ec protocol.EncryptedChange) (crdt.Change, error) {
	if err := ec.Validate(); err != nil {
		return crdt.Change{}, fmt.Errorf("statesync: refusing a change: %w", err)
	}
	raw, err := m.Decrypt(ec.SpaceID, ec.Ciphertext)
	if err != nil {
		return crdt.Change{}, err
	}
	var ch crdt.Change
	if err := json.Unmarshal(raw, &ch); err != nil {
		return crdt.Change{}, fmt.Errorf("statesync: decoding change: %w", err)
	}
	return ch, nil
}

// DecodeAll decodes a batch of encrypted changes into CRDT changes, ready to
// apply to a [crdt.State] in one merge. A change that fails to validate or
// decrypt fails the batch, because a hole would silently diverge the merge —
// better to surface it and re-sync than to apply a partial set as if complete.
func DecodeAll(m *client.Manager, changes []protocol.EncryptedChange) ([]crdt.Change, error) {
	out := make([]crdt.Change, 0, len(changes))
	for _, ec := range changes {
		ch, err := Decode(m, ec)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}
