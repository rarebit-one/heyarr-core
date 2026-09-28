package statesync

// snapshot.go bridges the client-side CRDT snapshot and the opaque encrypted
// snapshot the protocol moves (§44): a materialised state becomes an encrypted,
// content-addressed snapshot to ship; an encrypted snapshot fetched from a peer
// becomes a CRDT state to resume from, before the tail is applied. As with a
// change, the peer in between only ever sees ciphertext (Invariant 6).

import (
	"bytes"
	"errors"
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
	// The space and frontier ride INSIDE the ciphertext as well as beside it, so a
	// keyless holder cannot relabel the snapshot to a different causal point (#681).
	ct, err := m.Encrypt(spaceID, protocol.SealSnapshotPlaintext(spaceID, frontier, raw))
	if err != nil {
		return protocol.EncryptedSnapshot{}, err
	}
	return protocol.NewSnapshot(spaceID, frontier, ct)
}

// DecodeSnapshotWith validates an encrypted snapshot against its id (Invariant 1),
// decrypts it under the open space's key, checks the authenticated envelope
// against the snapshot's space and frontier, and reconstructs the CRDT state via
// the caller-supplied reconstructor — the starting point a joining device resumes
// from before applying the tail of changes after the snapshot's frontier. The
// reconstructor (crdt.FromSnapshot, crdt.DriveFromSnapshot, …) is what pins the
// kind; a device without the key cannot decrypt and never reaches it.
//
// A relabelled snapshot — valid ciphertext presented under a frontier or space it
// was not sealed with — is refused with [protocol.ErrSnapshotBindingMismatch]
// (#681). A legacy snapshot (sealed before the envelope) still decodes, because a
// key rotation may have left it as the only copy of the state; its frontier is
// unauthenticated, which [OpenSnapshot] reports to a caller that must know.
//
// A legacy plaintext must also be a CANONICAL snapshot of the kind: the state it
// reconstructs must re-serialise to exactly its bytes. Without the envelope,
// nothing else tells a snapshot from any other ciphertext sealed under the same
// key, and a keyless `write` holder could present a change's ciphertext as a
// snapshot; the permissive JSON decoders would read it as an empty state and hide
// everything the log has compacted away. A change is never a canonical snapshot,
// so it is refused with [ErrLegacySnapshotNotCanonical].
func DecodeSnapshotWith[T any](m *client.Manager, snap protocol.EncryptedSnapshot, from func([]byte) (T, error)) (T, error) {
	var zero T
	raw, authenticated, err := OpenSnapshot(m, snap)
	if err != nil {
		return zero, err
	}
	st, err := from(raw)
	if err != nil || authenticated {
		return st, err
	}
	s, ok := any(st).(Snapshotter)
	if !ok {
		return zero, fmt.Errorf("%w: %T cannot re-serialise to prove it", ErrLegacySnapshotNotCanonical, st)
	}
	again, err := s.Snapshot()
	if err != nil || !bytes.Equal(again, raw) {
		return zero, ErrLegacySnapshotNotCanonical
	}
	return st, nil
}

// ErrLegacySnapshotNotCanonical is a pre-envelope snapshot whose plaintext is not
// the canonical serialisation of the state it decodes to — most likely another
// record's ciphertext presented as a snapshot (#681).
var ErrLegacySnapshotNotCanonical = errors.New("statesync: legacy snapshot is not a canonical snapshot of its kind")

// OpenSnapshot validates, decrypts and unwraps a snapshot to its raw state bytes.
// authenticated is true only when the frontier and space were sealed inside the
// ciphertext and match the snapshot's outer fields — the condition for trusting
// the frontier to decide what a device may skip or what compaction may drop. It is
// false for a legacy (pre-envelope) snapshot, whose state is genuine but whose
// frontier is a claim anyone with a `write` token could have rewritten.
func OpenSnapshot(m *client.Manager, snap protocol.EncryptedSnapshot) (raw []byte, authenticated bool, err error) {
	if err := snap.Validate(); err != nil {
		return nil, false, fmt.Errorf("statesync: refusing a snapshot: %w", err)
	}
	pt, err := m.Decrypt(snap.SpaceID, snap.Ciphertext)
	if err != nil {
		return nil, false, err
	}
	raw, authenticated, err = protocol.OpenSnapshotPlaintext(snap, pt)
	if err != nil {
		return nil, false, fmt.Errorf("statesync: refusing a snapshot: %w", err)
	}
	return raw, authenticated, nil
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
