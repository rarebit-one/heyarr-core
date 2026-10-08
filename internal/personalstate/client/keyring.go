package client

// keyring.go is the client half of a space key's epochs (ADR-0103): sealing the
// previous key under the next when a space is rotated, and unrolling that chain
// back to epoch 0 when a space is opened. The peer stores each link as one opaque
// history row and can open none of them (Invariant 6).

import (
	"errors"
	"fmt"

	"github.com/rarebit-one/void-which-binds-go/encryption"
)

// A HistoryEntry is one link of a space's key chain as the peer serves it: the
// key of epoch Epoch-1 sealed under the key of epoch Epoch ([SealPrevious]).
type HistoryEntry struct {
	Epoch      int
	SealedPrev []byte
}

// ErrIncompleteHistory is a key chain with a link missing, duplicated or beyond
// the epoch being opened. Opening anyway would leave the content under the
// unreachable keys silently unreadable, so it is refused.
var ErrIncompleteHistory = errors.New("personalstate/client: the space's key history is incomplete")

// ErrNotCurrentKey is a key that does not open the newest history row, so it is
// not the space's current key — a stale recovery blob, or a forged one.
var ErrNotCurrentKey = errors.New("personalstate/client: the key is not the space's current key")

// SealPrevious seals prev under next — one history row (ADR-0103). The row is
// encryption.SealSpaceKey(key_N, key_{N-1}) from void-which-binds-go: the wire
// format every client shares, pinned by the library's key-chain vectors.
func SealPrevious(next, prev encryption.SpaceKey) ([]byte, error) {
	return encryption.SealSpaceKey(next, prev)
}

// OpenPrevious reverses [SealPrevious]: it opens a history row under the key of
// its epoch and returns the key of the epoch before. A row that is malformed, or
// sealed under a different key, is refused (encryption.ErrUnwrap).
func OpenPrevious(next encryption.SpaceKey, sealedPrev []byte) (encryption.SpaceKey, error) {
	return encryption.OpenSpaceKey(next, sealedPrev)
}

// Unroll walks a space's key chain from its current key, at epoch, back to
// epoch 0, returning every key newest first (so keys[i] is the key of
// epoch-i). Every row 1..epoch must be present exactly once, and none beyond
// epoch; a missing, duplicated or extra row is [ErrIncompleteHistory], and a row
// the chain's key does not open is refused — the current key is wrong, or the
// row was tampered with.
func Unroll(current encryption.SpaceKey, epoch int, history []HistoryEntry) ([]encryption.SpaceKey, error) {
	if current.IsZero() {
		return nil, errors.New("personalstate/client: the space key is not a usable key")
	}
	if epoch < 0 {
		return nil, fmt.Errorf("personalstate/client: key epoch %d is negative", epoch)
	}
	rows := make(map[int][]byte, len(history))
	for _, h := range history {
		if h.Epoch < 1 || h.Epoch > epoch {
			return nil, fmt.Errorf("%w: a row for epoch %d, opening at epoch %d", ErrIncompleteHistory, h.Epoch, epoch)
		}
		if _, dup := rows[h.Epoch]; dup {
			return nil, fmt.Errorf("%w: two rows for epoch %d", ErrIncompleteHistory, h.Epoch)
		}
		rows[h.Epoch] = h.SealedPrev
	}
	keys := make([]encryption.SpaceKey, 0, epoch+1)
	keys = append(keys, current)
	k := current
	for e := epoch; e >= 1; e-- {
		row, ok := rows[e]
		if !ok {
			return nil, fmt.Errorf("%w: no row for epoch %d (opening at epoch %d)", ErrIncompleteHistory, e, epoch)
		}
		prev, err := OpenPrevious(k, row)
		if err != nil {
			return nil, fmt.Errorf("personalstate/client: the key of epoch %d does not open its history row: %w", e, err)
		}
		keys = append(keys, prev)
		k = prev
	}
	return keys, nil
}

// VerifyCurrent checks that key is the CURRENT key of a space at epoch: it must
// open the newest history row. That is proof — only the real key of epoch opens
// the row sealed under it — so it refuses a stale key (one a rotation has
// replaced) and a forged one alike, with [ErrNotCurrentKey]. At epoch 0 there is
// no row and nothing to check; the caller needs another proof there.
func VerifyCurrent(key encryption.SpaceKey, epoch int, history []HistoryEntry) error {
	if epoch == 0 {
		return nil
	}
	for _, h := range history {
		if h.Epoch != epoch {
			continue
		}
		if _, err := OpenPrevious(key, h.SealedPrev); err != nil {
			return fmt.Errorf("%w (epoch %d)", ErrNotCurrentKey, epoch)
		}
		return nil
	}
	return fmt.Errorf("%w: no row for the current epoch %d", ErrIncompleteHistory, epoch)
}
