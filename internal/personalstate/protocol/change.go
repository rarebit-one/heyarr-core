package protocol

// change.go is the opaque change — the unit the encrypted-state sync protocol
// moves (§38, §42, §44). The peer sees exactly these fields and no more: the
// space a change belongs to, its content-addressed id, the causal parents that
// order it, and the ciphertext. It NEVER decrypts the ciphertext; the semantic
// merge is client-side (§42, Invariant 6). This is a SEPARATE protocol from CAS
// sync (§44) — its unit is a small encrypted causal change, not a large blob.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The errors this package refuses with.
var (
	// ErrIncomplete is a change missing a space, an id, or ciphertext. An empty
	// change references nothing and merges nothing.
	ErrIncomplete = errors.New("protocol: a change binding is empty")
	// ErrIDMismatch is a change whose stated id is not the id its bytes hash to —
	// a claimed id a destination must never trust (Invariant 1, ADR-0005 applied
	// to opaque changes). The peer recomputes the id and refuses a mismatch.
	ErrIDMismatch = errors.New("protocol: change id does not match its content")
)

// An EncryptedChange is one change as it crosses the peer surface and rests in a
// peer's store. Every field is what a peer is permitted to see (§38); the
// Ciphertext is opaque to it.
type EncryptedChange struct {
	// SpaceID is the space this change belongs to (§39).
	SpaceID string `json:"space_id"`
	// ChangeID is the content-addressed id, "blake3:<hex>", computed over the
	// space, the parents and the ciphertext — so two peers agree on it, it dedups,
	// and it cannot be forged to point a change's causal position elsewhere.
	ChangeID string `json:"change_id"`
	// Parents are the change ids this change causally follows — its parents in the
	// per-space DAG. A change with no parents is a root. Sorted, for a
	// deterministic id and encoding.
	Parents []string `json:"parents,omitempty"`
	// Ciphertext is the encrypted CRDT change (encryption.EncryptChange output),
	// opaque to every peer. The client decrypts and merges it (§42).
	Ciphertext []byte `json:"ciphertext"`
}

// NewChange mints a change: it sorts the parents, computes the content-addressed
// id, and returns the whole. The caller supplies the ciphertext (already
// encrypted client-side) and the causal parents (the space's current heads when
// the change was made).
func NewChange(spaceID string, parents []string, ciphertext []byte) (EncryptedChange, error) {
	if spaceID == "" || len(ciphertext) == 0 {
		return EncryptedChange{}, fmt.Errorf("%w: a change needs a space and ciphertext", ErrIncomplete)
	}
	sorted := canonicalParents(parents)
	return EncryptedChange{
		SpaceID:    spaceID,
		ChangeID:   computeID(spaceID, sorted, ciphertext),
		Parents:    sorted,
		Ciphertext: ciphertext,
	}, nil
}

// Validate re-derives the id from the change's own bytes and refuses a mismatch —
// the destination verifying identity itself rather than trusting a claimed id
// (Invariant 1, ADR-0005). A peer runs this on every change it receives, so a
// tampered id, a change re-pointed at a different space, or a re-parented change
// is rejected before it is stored.
func (c EncryptedChange) Validate() error {
	if c.SpaceID == "" || c.ChangeID == "" || len(c.Ciphertext) == 0 {
		return fmt.Errorf("%w: space=%q id=%q ciphertext=%dB", ErrIncomplete, c.SpaceID, c.ChangeID, len(c.Ciphertext))
	}
	want := computeID(c.SpaceID, canonicalParents(c.Parents), c.Ciphertext)
	if c.ChangeID != want {
		return fmt.Errorf("%w: stated %s, computed %s", ErrIDMismatch, c.ChangeID, want)
	}
	return nil
}

// Heads returns the causal frontier of a change set: the ids no other change in
// the set names as a parent. A device offers its heads and a peer pulls what it
// is missing beneath them — the incremental, resumable sync a phone needs (§46,
// #330), never a full-log replay. Order is deterministic (sorted).
func Heads(changes []EncryptedChange) []string {
	isParent := make(map[string]bool)
	ids := make(map[string]bool, len(changes))
	for _, c := range changes {
		ids[c.ChangeID] = true
		for _, p := range c.Parents {
			isParent[p] = true
		}
	}
	var heads []string
	for id := range ids {
		if !isParent[id] {
			heads = append(heads, id)
		}
	}
	sort.Strings(heads)
	return heads
}

// canonicalParents returns a sorted, de-duplicated, empty-free copy of parents,
// so the id and encoding do not depend on the order or repetition a caller passed.
func canonicalParents(parents []string) []string {
	if len(parents) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(parents))
	out := make([]string, 0, len(parents))
	for _, p := range parents {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// computeID hashes the space, the (already-canonical) parents and the ciphertext
// into a "blake3:<hex>" id. Every field is length-framed so no byte can migrate
// across a boundary and leave the digest unchanged — the same framing the pairing
// SAS uses — so a change cannot be re-pointed at a different space or re-parented
// without changing its id.
func computeID(spaceID string, parents []string, ciphertext []byte) string {
	h := hashing.New()
	writeField(h, []byte("heyarr/personalstate/change/v1"))
	writeField(h, []byte(spaceID))
	writeUvarint(h, uint64(len(parents)))
	for _, p := range parents {
		writeField(h, []byte(p))
	}
	writeField(h, ciphertext)
	return h.Sum().String()
}

func writeField(h *hashing.Hasher, b []byte) {
	writeUvarint(h, uint64(len(b)))
	_, _ = h.Write(b)
}

func writeUvarint(h *hashing.Hasher, n uint64) {
	var buf [binary.MaxVarintLen64]byte
	_, _ = h.Write(buf[:binary.PutUvarint(buf[:], n)])
}
