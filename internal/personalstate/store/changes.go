package store

// changes.go extends the peer store with encrypted CRDT changes (§42, §44) — the
// third opaque thing a peer holds, alongside spaces and wrapped keys. It stores a
// change under its content-addressed id and VERIFIES that id against the bytes
// before accepting it: a peer never trusts a claimed id (Invariant 1, ADR-0005).
// It never decrypts a change; the merge is client-side (§42).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// PutChange accepts an encrypted change into its space, idempotently. It first
// re-derives and checks the change's content-addressed id (protocol.Validate) —
// so a forged id, a change re-pointed at another space, or tampered ciphertext is
// refused before storage — then requires the space to exist, then inserts. A
// change already held is a no-op (the id is the primary key) and emits no event,
// so a re-sending relay cannot duplicate it.
func (s *Store) PutChange(ctx context.Context, ch protocol.EncryptedChange) error {
	if err := ch.Validate(); err != nil {
		return fmt.Errorf("personalstate/store: refusing a change: %w", err)
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists string
	err = tx.QueryRowContext(ctx, `SELECT id FROM encrypted_spaces WHERE id = ?`, ch.SpaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownSpace, ch.SpaceID)
	}
	if err != nil {
		return fmt.Errorf("personalstate/store: checking space: %w", err)
	}

	// seq is this peer's arrival order, assigned inside the transaction. Single
	// writer (ADR-0003), so MAX(seq)+1 cannot race. A re-delivered change hits the
	// ON CONFLICT and keeps its ORIGINAL seq — a device's cursor must never be
	// rewound by a relay re-sending something it already passed.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO encrypted_changes (change_id, space_id, parents, ciphertext, created_at, seq)
		 VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM encrypted_changes))
		 ON CONFLICT (change_id) DO NOTHING`,
		ch.ChangeID, ch.SpaceID, strings.Join(ch.Parents, ","), ch.Ciphertext, now.Format(timeFormat))
	if err != nil {
		return fmt.Errorf("personalstate/store: storing change: %w", err)
	}
	// Only a genuinely new change is a state transition worth an event; a
	// re-delivered one changes nothing.
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeChangeStored, "encrypted_change", ch.ChangeID,
		map[string]any{"space_id": ch.SpaceID})
	if err != nil {
		return fmt.Errorf("personalstate/store: recording change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return nil
}

// ChangesFor returns every change a peer holds for a space, oldest first — what a
// device pulls to merge. The space must exist.
func (s *Store) ChangesFor(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT change_id, space_id, parents, ciphertext
		 FROM encrypted_changes WHERE space_id = ? ORDER BY seq`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing changes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []protocol.EncryptedChange{}
	for rows.Next() {
		var c protocol.EncryptedChange
		var parents string
		if err := rows.Scan(&c.ChangeID, &c.SpaceID, &parents, &c.Ciphertext); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading change: %w", err)
		}
		if parents != "" {
			c.Parents = strings.Split(parents, ",")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangesSince returns the changes a space gained AFTER the arrival position
// [cursor], oldest first, together with the cursor to pass next time. It is the
// incremental form of [ChangesFor]: a device that has already merged everything
// up to its cursor pulls only the tail, so a steady-state sync transfers nothing
// instead of the whole log (§44).
//
// A cursor of 0 means "from the beginning" and makes this identical to
// ChangesFor. The returned cursor is the caller's next input; when no changes
// came back it is the cursor that went in, so a caller can store it
// unconditionally.
//
// Correctness rests on seq being append-only on this peer (see migration 00053):
// a change is only ever given a seq greater than every seq already stored, so
// nothing can appear BEHIND a cursor a device has already passed. Compaction
// (§44) deletes rows outright, which is safe here — a device past that point has
// already folded them, and the CRDT fold is idempotent, so a device that somehow
// re-receives one converges anyway.
//
// The cursor is this peer's local arrival order. It is opaque to the device and
// is NOT comparable across peers; a device that changes peer must start from 0.
func (s *Store) ChangesSince(ctx context.Context, spaceID string, cursor int64) ([]protocol.EncryptedChange, int64, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, 0, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT change_id, space_id, parents, ciphertext, seq
		 FROM encrypted_changes WHERE space_id = ? AND seq > ? ORDER BY seq`, spaceID, cursor)
	if err != nil {
		return nil, 0, fmt.Errorf("personalstate/store: listing changes since: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []protocol.EncryptedChange{}
	next := cursor
	for rows.Next() {
		var c protocol.EncryptedChange
		var parents string
		var seq int64
		if err := rows.Scan(&c.ChangeID, &c.SpaceID, &parents, &c.Ciphertext, &seq); err != nil {
			return nil, 0, fmt.Errorf("personalstate/store: reading change: %w", err)
		}
		if parents != "" {
			c.Parents = strings.Split(parents, ",")
		}
		out = append(out, c)
		next = seq
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

// HeadsFor returns the causal frontier of a space's changes — the heads a device
// offers so a peer can compute what it is missing (protocol.Missing). The space
// must exist.
func (s *Store) HeadsFor(ctx context.Context, spaceID string) ([]string, error) {
	changes, err := s.ChangesFor(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return protocol.Heads(changes), nil
}
