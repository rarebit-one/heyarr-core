package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// piecesSuffix names the record of which pieces of a sparsely-written partial
// have landed.
//
// It sits beside the staging file, under tmp/, and shares its prefix so that
// ReapTemp's age sweep takes both together — a bitset outliving its partial
// would describe a file that is not there, and a partial outliving its bitset
// is simply a transfer that starts over. Neither is worth a reference count.
const piecesSuffix = ".pieces"

// PieceProgressName is the file recording which pieces of h have landed.
func PieceProgressName(h hashing.Hash) string {
	return partialPrefix + h.Hex() + piecesSuffix
}

// SavePieceProgress records which pieces of a partial have landed.
//
// # It is a HINT and never evidence (ADR-0043)
//
// Publish re-reads the assembled file and hashes it whole, so a record that is
// wrong — by a bug, a torn write, or a crash between the write and this call —
// yields a digest mismatch and the transfer fails closed. It cannot produce a
// wrong blob. That is what lets this be written without ceremony: no fsync
// dance, no journal, no two-phase anything.
//
// What it buys is not correctness but a resume that starts from a better guess
// than zero, and what it costs when lost is a refetch.
//
// The encoding is whatever the caller hands over — this package does not know
// what a piece is, and deliberately: ADR-0041 keeps a piece a transport detail,
// and a CAS that could parse one would be a CAS that had learned.
func (s *FS) SavePieceProgress(blob hashing.Hash, encoded string) error {
	if blob.IsZero() {
		return errors.New("cas: refusing to record piece progress with no blob digest")
	}
	path := filepath.Join(s.root, tmpDir, PieceProgressName(blob))
	if strings.TrimSpace(encoded) == "" {
		// Nothing landed. Remove rather than write an empty file, so the
		// absence of a record and a record of nothing are the same state.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cas: clearing piece progress for %s: %w", blob, err)
		}
		return nil
	}
	// Written to a sibling and renamed over, for two reasons that are both
	// about this file being rewritten OFTEN — once per piece — rather than
	// produced once.
	//
	// A blob is immutable and is stored read-only (blobPerm). This is not a
	// blob: it is a temp record that is replaced on every piece, and writing it
	// read-only meant the FIRST save succeeded and every later one failed with
	// EACCES — silently, because losing a hint is not fatal. The visible
	// symptom was a peer that advertised its first piece and never any of the
	// others, which is §23 not happening rather than anything red.
	//
	// And the reader is another node. A peer serves from a partial while this
	// one fills it (ADR-0042), and its availability comes from this file — so a
	// truncate-and-rewrite in place has a window where a reader sees a shorter
	// bitset than either the old or the new one. A rename is atomic and has no
	// such window.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(encoded), tempPerm); err != nil {
		return s.permissionFault("recording piece progress", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cas: recording piece progress for %s: %w", blob, err)
	}
	return nil
}

// LoadPieceProgress returns what SavePieceProgress last recorded, or empty when
// there is nothing — which is the ordinary first-attempt case and not an error.
//
// The caller decides what to do with it, and ADR-0035's stance is unchanged: a
// resumed transfer trusts nothing it has not re-verified. This says where to
// look, not what is true.
func (s *FS) LoadPieceProgress(blob hashing.Hash) (string, error) {
	if blob.IsZero() {
		return "", errors.New("cas: refusing to read piece progress with no blob digest")
	}
	path := filepath.Join(s.root, tmpDir, PieceProgressName(blob))
	b, err := os.ReadFile(path) // #nosec G304 -- the name is derived from a validated digest
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cas: reading piece progress for %s: %w", blob, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// DiscardPieceProgress removes the record, for a transfer that has finished or
// given up. Missing is not an error — this is cleanup, and cleanup that fails
// on an absent file is cleanup that has to be guarded at every call site.
func (s *FS) DiscardPieceProgress(blob hashing.Hash) error {
	if blob.IsZero() {
		return nil
	}
	path := filepath.Join(s.root, tmpDir, PieceProgressName(blob))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: discarding piece progress for %s: %w", blob, err)
	}
	return nil
}

// ReadPartialAt reads from a blob's staging file WITHOUT taking it.
//
// # Why this cannot go through OpenPartial
//
// OpenPartial is exclusive — a second caller gets ErrPartialBusy — and it has
// to be, because two transfers appending to one file would interleave and both
// fail verification. But a swarm peer serves WHILE it fetches: that is the
// whole of §23, and the transfer holding the partial is exactly the one whose
// bytes another peer wants. An exclusive read would make a node unable to share
// precisely the blob it is working on.
//
// So this opens the file read-only and takes nothing. Concurrent reads and
// writes to DISJOINT regions of one file are safe, and disjointness is what the
// availability bitset establishes.
//
// # The ordering rule that makes it safe, stated so it cannot be lost
//
// A piece is recorded in the bitset AFTER its bytes are written, never before.
// So "the bitset says this piece landed" implies "those bytes are fully
// written", and a reader that consults the bitset first can never observe a
// half-written piece. Recording first would invert that and put torn bytes on
// the wire — where, unlike a local failure, they are somebody else's problem.
//
// This function does not consult the bitset itself, because it does not know
// what a piece is (ADR-0041). The caller must, and the caller is the one place
// where believing a bitset wrongly sends bad bytes to another peer rather than
// failing locally.
func (s *FS) ReadPartialAt(blob hashing.Hash, b []byte, off int64) (int, error) {
	if blob.IsZero() {
		return 0, errors.New("cas: refusing to read a staging file with no blob digest")
	}
	if off < 0 {
		return 0, fmt.Errorf("cas: refusing to read the staging file for %s at offset %d",
			blob, off)
	}
	path := filepath.Join(s.root, tmpDir, PartialName(blob))
	f, err := os.Open(path) // #nosec G304 -- the name is derived from a validated digest
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%w: no staging file for %s", ErrNotFound, blob)
		}
		return 0, fmt.Errorf("cas: opening the staging file for %s: %w", blob, err)
	}
	defer func() { _ = f.Close() }()
	return f.ReadAt(b, off)
}
