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
	if err := os.WriteFile(path, []byte(encoded), blobPerm); err != nil {
		return s.permissionFault("recording piece progress", path, err)
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
