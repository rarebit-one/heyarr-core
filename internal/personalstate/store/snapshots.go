package store

// snapshots.go extends the peer store with encrypted snapshots and compaction
// (§44) — the bounded-sync half of the state protocol. A snapshot is a
// materialised CRDT state at a causal point, stored under its content-addressed
// id as opaque ciphertext; compaction drops the changes a snapshot subsumes and
// every replica already holds, so a long-lived space is not an unbounded log. The
// peer never materialises a snapshot; it stores and serves ciphertext (§38).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// PutSnapshot accepts an encrypted snapshot into its space, idempotently. It
// re-derives and checks the content-addressed id (protocol.Validate) before
// storing — a peer never trusts a claimed id (Invariant 1) — then requires the
// space to exist. A snapshot already held is a no-op (the id is the primary key).
func (s *Store) PutSnapshot(ctx context.Context, snap protocol.EncryptedSnapshot) error {
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("personalstate/store: refusing a snapshot: %w", err)
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists string
	err = tx.QueryRowContext(ctx, `SELECT id FROM encrypted_spaces WHERE id = ?`, snap.SpaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownSpace, snap.SpaceID)
	}
	if err != nil {
		return fmt.Errorf("personalstate/store: checking space: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO encrypted_snapshots (snapshot_id, space_id, frontier, ciphertext, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (snapshot_id) DO NOTHING`,
		snap.SnapshotID, snap.SpaceID, strings.Join(snap.Frontier, ","), snap.Ciphertext, now.Format(timeFormat))
	if err != nil {
		return fmt.Errorf("personalstate/store: storing snapshot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSnapshotWritten, "encrypted_space", snap.SpaceID,
		map[string]any{"snapshot_id": snap.SnapshotID})
	if err != nil {
		return fmt.Errorf("personalstate/store: recording snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return nil
}

// LatestSnapshotFor returns the most recent snapshot a peer holds for a space,
// and whether one exists — what a joining device fetches before the tail. The
// space must exist.
func (s *Store) LatestSnapshotFor(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return protocol.EncryptedSnapshot{}, false, err
	}
	row := s.reader.QueryRowContext(ctx,
		`SELECT snapshot_id, space_id, frontier, ciphertext
		 FROM encrypted_snapshots WHERE space_id = ? ORDER BY created_at DESC, snapshot_id DESC LIMIT 1`, spaceID)
	var snap protocol.EncryptedSnapshot
	var frontier string
	err := row.Scan(&snap.SnapshotID, &snap.SpaceID, &frontier, &snap.Ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.EncryptedSnapshot{}, false, nil
	}
	if err != nil {
		return protocol.EncryptedSnapshot{}, false, fmt.Errorf("personalstate/store: reading snapshot: %w", err)
	}
	if frontier != "" {
		snap.Frontier = strings.Split(frontier, ",")
	}
	return snap, true, nil
}
