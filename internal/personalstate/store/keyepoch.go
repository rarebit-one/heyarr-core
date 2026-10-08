package store

// keyepoch.go is a space key's epoch and its opaque history (ADR-0103, #698).
// Epoch 0 is the key a space was created with; rotating to epoch N stores one
// row holding key_{N-1} sealed under key_N by the client. The peer cannot open a
// row (Invariant 6) — it stores and serves the bytes, and enforces only the
// structural rules that keep the chain linear: one row per epoch, rotation is a
// compare-and-swap on the epoch, and a wrap below the current epoch is refused
// and dropped. A current recipient unrolls the chain client-side.
//
// The current epoch is derived — MAX(epoch) of a space's history rows, 0 with
// none — never stored, so it cannot drift from the rows it summarises.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// A KeyHistoryEntry is one link of a space's key chain: the key of epoch
// Epoch-1, sealed under the key of epoch Epoch. SealedPrev is opaque.
type KeyHistoryEntry struct {
	SpaceID    string
	Epoch      int
	SealedPrev []byte
	CreatedAt  time.Time
}

// A RecipientWrap is one recipient's sealed copy of a new space key, as a
// rotation carries it. Wrapped is opaque (encryption.Seal output).
type RecipientWrap struct {
	Recipient string
	Wrapped   []byte
}

// KeyEpoch returns a space's current key epoch: 0 until its first rotation. The
// space must exist.
func (s *Store) KeyEpoch(ctx context.Context, spaceID string) (int, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return 0, err
	}
	var epoch int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(epoch), 0) FROM space_key_history WHERE space_id = ?`, spaceID).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("personalstate/store: reading key epoch: %w", err)
	}
	return epoch, nil
}

// KeyState returns a space's current key epoch and its wrapped copies from ONE
// read transaction, so the pair is consistent: a rotation committing between two
// separate reads would otherwise pair the new epoch with the old wraps (or the
// reverse). The space must exist.
func (s *Store) KeyState(ctx context.Context, spaceID string) (int, []WrappedKey, error) {
	tx, err := s.reader.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("personalstate/store: beginning read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := spaceExistsTx(ctx, tx, spaceID); err != nil {
		if errors.Is(err, ErrUnknownSpace) {
			return 0, nil, ErrUnknownSpace
		}
		return 0, nil, err
	}
	epoch, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return 0, nil, err
	}
	keys, err := wrappedKeysTx(ctx, tx, spaceID)
	if err != nil {
		return 0, nil, err
	}
	return epoch, keys, tx.Commit()
}

// KeyHistory returns a space's key chain, oldest epoch first — what a device
// fetches to read content sealed before the current key. The space must exist.
func (s *Store) KeyHistory(ctx context.Context, spaceID string) ([]KeyHistoryEntry, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT space_id, epoch, sealed_prev, created_at
		 FROM space_key_history WHERE space_id = ? ORDER BY epoch`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing key history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []KeyHistoryEntry{}
	for rows.Next() {
		var e KeyHistoryEntry
		var created string
		if err := rows.Scan(&e.SpaceID, &e.Epoch, &e.SealedPrev, &created); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading key history: %w", err)
		}
		if e.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
			return nil, fmt.Errorf("personalstate/store: key history %s/%d has an unparseable created_at: %w", e.SpaceID, e.Epoch, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RotateKey moves a space to the next key epoch in one transaction (ADR-0103):
// it stores sealedPrev — the current key sealed under the new one, by the client
// — as the history row for expectedEpoch+1, upserts every wrap at the new epoch,
// and deletes every wrap of the space below it. That last step is how revocation
// lands: a recipient the rotation did not re-wrap loses its copy.
//
// It is a compare-and-swap: the space's current epoch must equal expectedEpoch,
// or it is ErrKeyEpochConflict and nothing is written, so two concurrent
// rotations cannot fork the chain. A rotation needs non-empty sealedPrev and at
// least one wrap. Returns the new epoch.
//
// preserve names recipients a rotation may not silently drop — the recovery
// keys (ADR-0022). Any of them holding a wrap at the current epoch must be among
// wraps, or it is ErrRotationDropsPreserved and nothing is written: the history
// seals backwards only, so a recovery key left on the old key could never reach
// the new one, and offline recovery would be lost without anyone noticing. The
// check runs inside the transaction, against the wraps the rotation replaces.
func (s *Store) RotateKey(ctx context.Context, spaceID string, expectedEpoch int, sealedPrev []byte, wraps []RecipientWrap, preserve map[string]bool) (int, error) {
	if expectedEpoch < 0 {
		return 0, fmt.Errorf("%w: expected epoch %d", ErrInvalidKeyEpoch, expectedEpoch)
	}
	if len(sealedPrev) == 0 {
		return 0, ErrEmptySealedPrev
	}
	if len(wraps) == 0 {
		return 0, ErrNoRotationWraps
	}
	for _, w := range wraps {
		if w.Recipient == "" {
			return 0, ErrEmptyRecipient
		}
		if len(w.Wrapped) == 0 {
			return 0, ErrEmptyWrapped
		}
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := spaceExistsTx(ctx, tx, spaceID); err != nil {
		return 0, err
	}
	current, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return 0, err
	}
	if current != expectedEpoch {
		return 0, fmt.Errorf("%w: space %s is at epoch %d, the rotation expected %d",
			ErrKeyEpochConflict, spaceID, current, expectedEpoch)
	}
	if len(preserve) > 0 {
		held, err := wrappedKeysTx(ctx, tx, spaceID)
		if err != nil {
			return 0, err
		}
		rewrapping := make(map[string]bool, len(wraps))
		for _, w := range wraps {
			rewrapping[w.Recipient] = true
		}
		for _, h := range held {
			if h.Epoch == current && preserve[h.Recipient] && !rewrapping[h.Recipient] {
				return 0, fmt.Errorf("%w: %s", ErrRotationDropsPreserved, h.Recipient)
			}
		}
	}
	next := expectedEpoch + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO space_key_history (space_id, epoch, sealed_prev, created_at) VALUES (?, ?, ?, ?)`,
		spaceID, next, sealedPrev, now.Format(timeFormat)); err != nil {
		return 0, fmt.Errorf("personalstate/store: storing key history: %w", err)
	}

	var evs []events.Event
	rewrapped := make(map[string]bool, len(wraps))
	for _, w := range wraps {
		ev, _, err := s.upsertWrapTx(ctx, tx, spaceID, w.Recipient, w.Wrapped, next, now)
		if err != nil {
			return 0, err
		}
		evs = append(evs, ev)
		rewrapped[w.Recipient] = true
	}
	dropped, err := s.dropStaleWrapsTx(ctx, tx, spaceID, next)
	if err != nil {
		return 0, err
	}
	for _, r := range dropped {
		ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyRevoked, "encrypted_space", spaceID,
			map[string]any{"recipient": r, "epoch": next})
		if err != nil {
			return 0, fmt.Errorf("personalstate/store: recording revocation: %w", err)
		}
		evs = append(evs, ev)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyRotated, "encrypted_space", spaceID,
		map[string]any{"epoch": next, "recipients": len(rewrapped), "revoked": len(dropped)})
	if err != nil {
		return 0, fmt.Errorf("personalstate/store: recording rotation: %w", err)
	}
	evs = append(evs, ev)
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(evs...)
	return next, nil
}

// PutKeyHistory accepts one history row by REPLICATION (§45): a sibling that
// rotated (or received a rotation) pushes the row so this peer can serve the
// chain. It is idempotent — the same bytes for an epoch already held is a no-op
// with no event (Invariant 9) — and a DIFFERENT row for a held epoch is
// ErrKeyHistoryConflict: a fork is surfaced, never overwritten.
//
// When the row becomes the space's newest epoch, every wrap below it is deleted,
// exactly as RotateKey does on the peer the rotation happened on — so a
// revocation replicates as the absence of a re-wrap, not as a delete message
// that a stale peer could miss. Rows arrive in ascending epoch order from
// replication; an older row arriving later fills its link and drops nothing.
func (s *Store) PutKeyHistory(ctx context.Context, spaceID string, epoch int, sealedPrev []byte) error {
	if epoch < 1 {
		return fmt.Errorf("%w: a history row is for epoch >= 1, got %d", ErrInvalidKeyEpoch, epoch)
	}
	if len(sealedPrev) == 0 {
		return ErrEmptySealedPrev
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := spaceExistsTx(ctx, tx, spaceID); err != nil {
		return err
	}
	var held []byte
	err = tx.QueryRowContext(ctx,
		`SELECT sealed_prev FROM space_key_history WHERE space_id = ? AND epoch = ?`, spaceID, epoch).Scan(&held)
	switch {
	case err == nil:
		if bytes.Equal(held, sealedPrev) {
			return tx.Commit() // already held — idempotent, no event
		}
		return fmt.Errorf("%w: space %s epoch %d", ErrKeyHistoryConflict, spaceID, epoch)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("personalstate/store: checking key history: %w", err)
	}

	current, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO space_key_history (space_id, epoch, sealed_prev, created_at) VALUES (?, ?, ?, ?)`,
		spaceID, epoch, sealedPrev, now.Format(timeFormat)); err != nil {
		return fmt.Errorf("personalstate/store: storing key history: %w", err)
	}
	var evs []events.Event
	var dropped []string
	if epoch > current {
		if dropped, err = s.dropStaleWrapsTx(ctx, tx, spaceID, epoch); err != nil {
			return err
		}
		for _, r := range dropped {
			ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyRevoked, "encrypted_space", spaceID,
				map[string]any{"recipient": r, "epoch": epoch})
			if err != nil {
				return fmt.Errorf("personalstate/store: recording revocation: %w", err)
			}
			evs = append(evs, ev)
		}
	}
	// A row that becomes the newest epoch is a rotation arriving by replication.
	// An older one only fills a missing link (current epoch and wraps untouched),
	// so it gets its own event rather than a rotation event for a stale epoch.
	typ, payload := events.TypeSpaceKeyRotated, map[string]any{"epoch": epoch, "replicated": true, "revoked": len(dropped)}
	if epoch <= current {
		typ, payload = events.TypeSpaceKeyHistoryBackfilled, map[string]any{"epoch": epoch, "current_epoch": current}
	}
	ev, err := s.events.EmitTx(ctx, tx, typ, "encrypted_space", spaceID, payload)
	if err != nil {
		return fmt.Errorf("personalstate/store: recording key history: %w", err)
	}
	evs = append(evs, ev)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(evs...)
	return nil
}

// dropStaleWrapsTx deletes every wrap of a space below epoch and returns the
// recipients whose copy went (sorted, for stable events).
func (s *Store) dropStaleWrapsTx(ctx context.Context, tx *sql.Tx, spaceID string, epoch int) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`DELETE FROM wrapped_keys WHERE space_id = ? AND epoch < ? RETURNING recipient`, spaceID, epoch)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: dropping superseded wraps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("personalstate/store: dropping superseded wraps: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// keyEpochTx is KeyEpoch inside a write transaction, so the check and the write
// that depends on it see the same state.
func keyEpochTx(ctx context.Context, tx *sql.Tx, spaceID string) (int, error) {
	var epoch int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(epoch), 0) FROM space_key_history WHERE space_id = ?`, spaceID).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("personalstate/store: reading key epoch: %w", err)
	}
	return epoch, nil
}

func spaceExistsTx(ctx context.Context, tx *sql.Tx, spaceID string) error {
	var exists string
	err := tx.QueryRowContext(ctx, `SELECT id FROM encrypted_spaces WHERE id = ?`, spaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownSpace, spaceID)
	}
	if err != nil {
		return fmt.Errorf("personalstate/store: checking space: %w", err)
	}
	return nil
}
