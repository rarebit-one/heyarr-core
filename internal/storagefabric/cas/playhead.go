package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// playheadSuffix names the record of where a consumer is reading in a blob that
// is still assembling — a single byte offset, beside the staging file under tmp/.
//
// It shares the partial's prefix so ReapTemp's age sweep takes it with the
// partial: a playhead outliving its transfer describes a read nobody is doing,
// and a transfer outliving its playhead simply fetches without a hint. Neither is
// worth a reference count, exactly as for the availability record it sits beside.
const playheadSuffix = ".playhead"

// PlayheadName is the file recording where a consumer is reading in blob h.
func PlayheadName(h hashing.Hash) string {
	return partialPrefix + h.Hex() + playheadSuffix
}

// SavePlayhead records the byte offset a consumer is currently reading in a blob
// that is still arriving, so the transfer can prioritise the pieces near it
// (§33, §84). It is a HINT and nothing more: it steers WHICH missing piece is
// fetched next, never whether a piece is trusted, so a wrong or stale value costs
// a worse fetch order and never a wrong byte.
//
// It crosses a role boundary the only way invariant 4 allows — through the store,
// not a shared pointer. The reader writes here (API role) and the transfer reads
// it (worker role); both reach the same tmp/ directory, exactly as they already
// do for the availability record the transfer writes and the reader consults.
//
// Written to a sibling and renamed over, like the availability record and for the
// same reason: it is rewritten as the read position moves, so a truncate-in-place
// would give a concurrent reader a torn value. A negative offset is refused; the
// caller has no business reading before the start of a blob.
func (s *FS) SavePlayhead(blob hashing.Hash, offset int64) error {
	if blob.IsZero() {
		return errors.New("cas: refusing to record a playhead with no blob digest")
	}
	if offset < 0 {
		return fmt.Errorf("cas: refusing to record a negative playhead %d for %s", offset, blob)
	}
	path := filepath.Join(s.root, tmpDir, PlayheadName(blob))
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), tempPerm); err != nil {
		return s.permissionFault("recording a playhead", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cas: recording a playhead for %s: %w", blob, err)
	}
	return nil
}

// LoadPlayhead returns the offset SavePlayhead last recorded and ok=true, or
// ok=false when there is none — which is the ordinary case for a transfer no
// consumer is reading, and not an error. Like the availability record it says
// where to look, not what is true (ADR-0043).
func (s *FS) LoadPlayhead(blob hashing.Hash) (offset int64, ok bool, err error) {
	if blob.IsZero() {
		return 0, false, errors.New("cas: refusing to read a playhead with no blob digest")
	}
	path := filepath.Join(s.root, tmpDir, PlayheadName(blob))
	b, err := os.ReadFile(path) // #nosec G304 -- the name is derived from a validated digest
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("cas: reading a playhead for %s: %w", blob, err)
	}
	off, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if perr != nil {
		// A garbled record is a hint we cannot read, treated as absent rather than
		// as a fault — the transfer simply fetches without it.
		return 0, false, nil
	}
	return off, true, nil
}

// DiscardPlayhead removes the record, for a consumer that has stopped reading.
// Missing is not an error — this is cleanup.
func (s *FS) DiscardPlayhead(blob hashing.Hash) error {
	if blob.IsZero() {
		return nil
	}
	path := filepath.Join(s.root, tmpDir, PlayheadName(blob))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: discarding a playhead for %s: %w", blob, err)
	}
	return nil
}
