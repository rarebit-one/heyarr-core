package protocol

// snapshot_envelope.go is the authenticated plaintext a snapshot's ciphertext
// carries (§44, #681). A snapshot's Frontier and SnapshotID travel OUTSIDE the
// ciphertext, and the id is a public BLAKE3 digest anyone can recompute. So
// without this envelope, a principal holding only a `write` token and no space
// key could fetch a valid snapshot, swap its frontier for the current heads,
// recompute the id and push it back. It would still decrypt, and a device or an
// operator trusting that frontier would treat changes the state never folded as
// subsumed.
//
// The fix binds (record type, space, frontier) INSIDE the AEAD: the plaintext a
// snapshot producer encrypts is this envelope, not the bare state. The content
// cipher (voidbind-go encryption.EncryptChange) takes no associated data, so the
// binding rides in the authenticated plaintext instead — the same guarantee,
// since Poly1305 authenticates every plaintext byte. After decryption a reader
// re-checks the envelope against the snapshot's outer fields and refuses a
// mismatch. The peer never sees any of it (Invariant 6).
//
// Wire form, using the same length framing as the content-addressed ids
// (field(b) = uvarint(len b) ‖ b, uvarint = encoding/binary.PutUvarint):
//
//	field("heyarr/personalstate/snapshot-envelope/v2")   record type + version
//	field(space id)
//	uvarint(len frontier) ‖ field(head)…                  canonical frontier
//	field(state)                                          Snapshotter bytes
//
// nothing may follow. A snapshot written before this envelope existed (v1) is
// the bare state, which every CRDT serialises as a JSON object, so it can never
// begin with the envelope's first field. [OpenSnapshotPlaintext] still accepts
// such a legacy snapshot — a key rotation leaves one as the only copy of a
// space's state — but reports it unauthenticated, so anything that would TRUST
// its frontier (cold start from the tail, compaction) can refuse it.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// SnapshotEnvelopeDomain is the record type and version bound into every
// snapshot's authenticated plaintext. It differs from the change and snapshot id
// domains, so an envelope and an id preimage are unrelated byte strings.
const SnapshotEnvelopeDomain = "heyarr/personalstate/snapshot-envelope/v2"

// The errors the envelope refuses with.
var (
	// ErrSnapshotBindingMismatch is a snapshot whose authenticated envelope names
	// a different space or frontier from the one it travels under: it was
	// relabelled by someone who could not re-encrypt it (#681).
	ErrSnapshotBindingMismatch = errors.New("protocol: snapshot frontier or space does not match its authenticated envelope")
	// ErrSnapshotEnvelopeMalformed is an envelope that starts with the v2 domain
	// but does not parse to exactly its fields.
	ErrSnapshotEnvelopeMalformed = errors.New("protocol: snapshot envelope is malformed")
)

// envelopeMagic is the encoded first field of every v2 envelope — how a reader
// tells it from a legacy bare-JSON snapshot.
var envelopeMagic = appendField(nil, []byte(SnapshotEnvelopeDomain))

// SealSnapshotPlaintext builds the authenticated plaintext for a snapshot of
// state taken at frontier in spaceID: the bytes a producer encrypts under the
// space key. The frontier is canonicalised exactly as [NewSnapshot] does, so the
// envelope and the outer snapshot agree whatever order the caller passed.
func SealSnapshotPlaintext(spaceID string, frontier []string, state []byte) []byte {
	canon := canonicalParents(frontier)
	out := append([]byte(nil), envelopeMagic...)
	out = appendField(out, []byte(spaceID))
	out = binary.AppendUvarint(out, uint64(len(canon)))
	for _, f := range canon {
		out = appendField(out, []byte(f))
	}
	return appendField(out, state)
}

// OpenSnapshotPlaintext checks a decrypted snapshot plaintext against the outer
// snapshot it arrived in and returns the state bytes inside it. authenticated is
// true for a v2 envelope whose space and frontier match snap's — the only case in
// which snap.Frontier may be trusted. A v2 envelope that does NOT match is refused
// with [ErrSnapshotBindingMismatch]. A legacy plaintext (no envelope) is returned
// whole with authenticated false: its state is genuine, since it decrypted under
// the space key, but its frontier is only a claim.
func OpenSnapshotPlaintext(snap EncryptedSnapshot, plaintext []byte) (state []byte, authenticated bool, err error) {
	if !bytes.HasPrefix(plaintext, envelopeMagic) {
		return plaintext, false, nil
	}
	r := plaintext[len(envelopeMagic):]
	space, r, err := readField(r)
	if err != nil {
		return nil, false, err
	}
	n, k := binary.Uvarint(r)
	if k <= 0 || n > uint64(len(r)) {
		return nil, false, fmt.Errorf("%w: frontier count", ErrSnapshotEnvelopeMalformed)
	}
	r = r[k:]
	frontier := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		var f []byte
		if f, r, err = readField(r); err != nil {
			return nil, false, err
		}
		frontier = append(frontier, string(f))
	}
	if state, r, err = readField(r); err != nil {
		return nil, false, err
	}
	if len(r) != 0 {
		return nil, false, fmt.Errorf("%w: %d trailing byte(s)", ErrSnapshotEnvelopeMalformed, len(r))
	}
	if string(space) != snap.SpaceID || !slices.Equal(frontier, canonicalParents(snap.Frontier)) {
		return nil, false, fmt.Errorf("%w: sealed for space %q at %d head(s), presented as space %q at %d head(s)",
			ErrSnapshotBindingMismatch, space, len(frontier), snap.SpaceID, len(snap.Frontier))
	}
	return state, true, nil
}

func appendField(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func readField(r []byte) (field, rest []byte, err error) {
	n, k := binary.Uvarint(r)
	if k <= 0 {
		return nil, nil, fmt.Errorf("%w: truncated field length", ErrSnapshotEnvelopeMalformed)
	}
	r = r[k:]
	if n > uint64(len(r)) {
		return nil, nil, fmt.Errorf("%w: truncated field", ErrSnapshotEnvelopeMalformed)
	}
	return r[:n], r[n:], nil
}
