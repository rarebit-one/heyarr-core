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

// Snapshotter is any materialised CRDT state that serialises to canonical snapshot
// bytes. Every personal-state CRDT satisfies it — the playlist [crdt.State], and
// (issue #538) the vault [crdt.Drive] — so one snapshot bridge carries them all,
// exactly as [EncodeChange] carries every change type. The interface is the only
// thing this package needs to know about a state to make it opaque and shippable.
type Snapshotter interface {
	Snapshot() ([]byte, error)
}

// EncodeSnapshotOf serialises ANY [Snapshotter] CRDT state, encrypts it under an
// open space's key, and wraps it as a content-addressed [protocol.EncryptedSnapshot]
// at the given causal frontier — ready to push to a peer. The space must be open on
// m. It is generic so a new CRDT kind rides the snapshot bridge with no new encrypt
// path, and the peer in between still only sees ciphertext (Invariant 6).
func EncodeSnapshotOf[T Snapshotter](m *client.Manager, spaceID string, frontier []string, st T) (protocol.EncryptedSnapshot, error) {
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

// DecodeSnapshotWith validates an encrypted snapshot against its id (Invariant 1),
// decrypts it under the open space's key, and reconstructs the CRDT state via the
// caller-supplied reconstructor — the starting point a joining device resumes from
// before applying the tail of changes after the snapshot's frontier. The
// reconstructor (crdt.FromSnapshot, crdt.DriveFromSnapshot, …) is what pins the
// kind; a device without the key cannot decrypt and never reaches it.
func DecodeSnapshotWith[T any](m *client.Manager, snap protocol.EncryptedSnapshot, from func([]byte) (T, error)) (T, error) {
	var zero T
	if err := snap.Validate(); err != nil {
		return zero, fmt.Errorf("statesync: refusing a snapshot: %w", err)
	}
	raw, err := m.Decrypt(snap.SpaceID, snap.Ciphertext)
	if err != nil {
		return zero, err
	}
	return from(raw)
}

// EncodeSnapshot is the playlist-state snapshot bridge, kept as the original entry
// point every current caller uses. It is [EncodeSnapshotOf] pinned to [crdt.State].
func EncodeSnapshot(m *client.Manager, spaceID string, frontier []string, st *crdt.State) (protocol.EncryptedSnapshot, error) {
	return EncodeSnapshotOf(m, spaceID, frontier, st)
}

// DecodeSnapshot is the playlist-state snapshot bridge, kept for existing callers.
// It is [DecodeSnapshotWith] pinned to [crdt.FromSnapshot].
func DecodeSnapshot(m *client.Manager, snap protocol.EncryptedSnapshot) (*crdt.State, error) {
	return DecodeSnapshotWith(m, snap, crdt.FromSnapshot)
}

// EncodeDriveSnapshot is the vault-drive snapshot bridge (issue #538): it is
// [EncodeSnapshotOf] pinned to [crdt.Drive], the drive counterpart of the playlist
// [EncodeSnapshot], so a drive snapshot ships on the same opaque, encrypted path.
func EncodeDriveSnapshot(m *client.Manager, spaceID string, frontier []string, d *crdt.Drive) (protocol.EncryptedSnapshot, error) {
	return EncodeSnapshotOf(m, spaceID, frontier, d)
}

// DecodeDriveSnapshot is the vault-drive snapshot bridge: [DecodeSnapshotWith]
// pinned to [crdt.DriveFromSnapshot], the drive counterpart of [DecodeSnapshot].
func DecodeDriveSnapshot(m *client.Manager, snap protocol.EncryptedSnapshot) (*crdt.Drive, error) {
	return DecodeSnapshotWith(m, snap, crdt.DriveFromSnapshot)
}
