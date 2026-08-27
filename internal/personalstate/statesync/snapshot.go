package statesync

// snapshot.go bridges the client-side CRDT snapshot and the opaque encrypted
// snapshot the protocol moves (§44): a materialised state becomes an encrypted,
// content-addressed snapshot to ship; an encrypted snapshot fetched from a peer
// becomes a CRDT state to resume from, before the tail is applied. As with a
// change, the peer in between only ever sees ciphertext (Invariant 6).

import (
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// EncodeSnapshot serialises a CRDT state, encrypts it under an open space's key,
// and wraps it as a content-addressed [protocol.EncryptedSnapshot] at the given
// causal frontier — ready to push to a peer. The space must be open on m.
func EncodeSnapshot(m *client.Manager, spaceID string, frontier []string, st *crdt.State) (protocol.EncryptedSnapshot, error) {
	raw, err := st.Snapshot()
	if err != nil {
		return protocol.EncryptedSnapshot{}, fmt.Errorf("statesync: serialising snapshot: %w", err)
	}
	ct, err := m.Encrypt(spaceID, raw)
	if err != nil {
		return protocol.EncryptedSnapshot{}, err
	}
	return protocol.NewSnapshot(spaceID, frontier, ct)
}

// DecodeSnapshot validates an encrypted snapshot against its id (Invariant 1),
// decrypts it under the open space's key, and reconstructs the CRDT state — the
// starting point a joining device resumes from before applying the tail of
// changes after the snapshot's frontier. A device without the key cannot decode.
func DecodeSnapshot(m *client.Manager, snap protocol.EncryptedSnapshot) (*crdt.State, error) {
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("statesync: refusing a snapshot: %w", err)
	}
	raw, err := m.Decrypt(snap.SpaceID, snap.Ciphertext)
	if err != nil {
		return nil, err
	}
	st, err := crdt.FromSnapshot(raw)
	if err != nil {
		return nil, err
	}
	return st, nil
}
