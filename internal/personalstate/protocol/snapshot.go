package protocol

// snapshot.go is the opaque encrypted snapshot (§44): a materialised CRDT state
// at a causal point, encrypted under the space key, that a joining or long-offline
// device fetches instead of the whole change log. Like a change it is opaque to
// the peer — a space, a content-addressed id, the causal frontier it covers, and
// ciphertext — and the peer never decrypts it. It is a SEPARATE record from a
// change: a change is one delta, a snapshot folds many.

import (
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The errors this file refuses with.
var (
	// ErrSnapshotIncomplete is a snapshot missing a space, an id, or ciphertext.
	ErrSnapshotIncomplete = errors.New("protocol: a snapshot binding is empty")
	// ErrSnapshotIDMismatch is a snapshot whose stated id is not the id its bytes
	// hash to — a claimed id a peer must never trust (Invariant 1, ADR-0005).
	ErrSnapshotIDMismatch = errors.New("protocol: snapshot id does not match its content")
)

// An EncryptedSnapshot is a materialised state at a causal point, as the peer
// holds it. Frontier is the set of change heads the snapshot subsumes: a device
// reaching state from this snapshot then applies only the changes NOT in the
// frontier's causal history (the tail).
type EncryptedSnapshot struct {
	// SpaceID is the space this snapshot belongs to (§39).
	SpaceID string `json:"space_id"`
	// SnapshotID is the content-addressed id, "blake3:<hex>", over the space, the
	// frontier and the ciphertext — so two peers agree on it and it cannot be
	// forged to claim a different causal point.
	SnapshotID string `json:"snapshot_id"`
	// Frontier is the causal heads this snapshot materialises — the point after
	// which the tail begins. Sorted, for a deterministic id.
	Frontier []string `json:"frontier"`
	// Ciphertext is the encrypted materialised state (encryption.EncryptChange
	// over crdt.State.Snapshot()), opaque to every peer.
	Ciphertext []byte `json:"ciphertext"`
}

// NewSnapshot mints a snapshot: it sorts the frontier, computes the
// content-addressed id, and returns the whole. The caller supplies the ciphertext
// (the encrypted materialised state) and the frontier it was taken at.
func NewSnapshot(spaceID string, frontier []string, ciphertext []byte) (EncryptedSnapshot, error) {
	if spaceID == "" || len(ciphertext) == 0 {
		return EncryptedSnapshot{}, fmt.Errorf("%w: a snapshot needs a space and ciphertext", ErrSnapshotIncomplete)
	}
	sorted := canonicalParents(frontier)
	return EncryptedSnapshot{
		SpaceID:    spaceID,
		SnapshotID: computeSnapshotID(spaceID, sorted, ciphertext),
		Frontier:   sorted,
		Ciphertext: ciphertext,
	}, nil
}

// Validate re-derives the id from the snapshot's own bytes and refuses a mismatch
// — the destination verifying identity itself (Invariant 1, ADR-0005).
func (s EncryptedSnapshot) Validate() error {
	if s.SpaceID == "" || s.SnapshotID == "" || len(s.Ciphertext) == 0 {
		return fmt.Errorf("%w: space=%q id=%q ciphertext=%dB", ErrSnapshotIncomplete, s.SpaceID, s.SnapshotID, len(s.Ciphertext))
	}
	want := computeSnapshotID(s.SpaceID, canonicalParents(s.Frontier), s.Ciphertext)
	if s.SnapshotID != want {
		return fmt.Errorf("%w: stated %s, computed %s", ErrSnapshotIDMismatch, s.SnapshotID, want)
	}
	return nil
}

// Subsumes reports whether a change is covered by this snapshot's frontier — i.e.
// the change is in the causal history the snapshot already folded in, so a device
// starting from the snapshot does not need it. It is pure DAG reachability over
// the change set: a change reachable from the frontier is subsumed.
//
// have is the full change set the caller holds (needed to walk parents). A change
// whose ancestry cannot be confirmed present is treated as NOT subsumed — the
// safe bias, because dropping a change a peer still needs is data loss (§44).
func (s EncryptedSnapshot) Subsumes(have []EncryptedChange, changeID string) bool {
	return CausalHistory(have, s.Frontier)[changeID]
}

// computeSnapshotID hashes the space, the frontier and the ciphertext into a
// "blake3:<hex>" id, under a domain distinct from a change's, so a snapshot and a
// change over the same bytes are unrelated values.
func computeSnapshotID(spaceID string, frontier []string, ciphertext []byte) string {
	h := hashing.New()
	writeField(h, []byte("heyarr/personalstate/snapshot/v1"))
	writeField(h, []byte(spaceID))
	writeUvarint(h, uint64(len(frontier)))
	for _, f := range frontier {
		writeField(h, []byte(f))
	}
	writeField(h, ciphertext)
	return h.Sum().String()
}
