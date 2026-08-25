package backupsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// timestampFormat is how instants are stored, matching the rest of the schema.
const timestampFormat = time.RFC3339Nano

// Clock is injected so a distribution cycle's timestamps do not depend on wall
// time (ADR-0017). It matches events.Clock and backup.Clock.
type Clock interface{ Now() time.Time }

// Beliefs records what generation each peer is believed to hold of THIS node's
// control-plane backup (§50, ADR-0046). It is a belief, authoritative only until
// the peer can be asked — see 00033_peer_control_backups.sql for why a belief is
// stored at all.
type Beliefs struct {
	db *sqlite.DB
}

// NewBeliefs reads and writes the belief table on the controller database.
func NewBeliefs(db *sqlite.DB) *Beliefs { return &Beliefs{db: db} }

// Belief is one peer's believed state.
type Belief struct {
	PeerID     string
	Generation int64
	Digest     string
	PushedAt   time.Time
}

// Record upserts the belief that peerID now holds this generation.
//
// It refuses to move a peer's generation backwards: a confirmed push is
// monotonic (a backup generation is, ADR-0044), so a lower value arriving later
// is a stale response or a bug, never a fact — the same guard the catalog
// snapshot applies to its version.
func (b *Beliefs) Record(ctx context.Context, peerID string, generation int64, digest string, at time.Time) error {
	if generation <= 0 {
		return fmt.Errorf("backupsync: a believed generation must be positive, got %d", generation)
	}
	_, err := b.db.Writer().ExecContext(ctx, `
		INSERT INTO peer_control_backups (peer_id, generation, digest, pushed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (peer_id) DO UPDATE SET
			generation = excluded.generation,
			digest     = excluded.digest,
			pushed_at  = excluded.pushed_at
		WHERE excluded.generation >= peer_control_backups.generation`,
		peerID, generation, digest, at.UTC().Format(timestampFormat))
	if err != nil {
		return fmt.Errorf("backupsync: recording the belief for %s: %w", peerID, err)
	}
	return nil
}

// Of reports the generation believed held by peerID. The boolean is false when
// nothing has been pushed to that peer yet — distinct from generation zero,
// which the schema forbids.
func (b *Beliefs) Of(ctx context.Context, peerID string) (int64, bool, error) {
	var gen int64
	err := b.db.Reader().QueryRowContext(ctx,
		`SELECT generation FROM peer_control_backups WHERE peer_id = ?`, peerID).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("backupsync: reading the belief for %s: %w", peerID, err)
	}
	return gen, true, nil
}

// All returns every peer's belief, for the operator-facing "who is behind" view.
func (b *Beliefs) All(ctx context.Context) ([]Belief, error) {
	rows, err := b.db.Reader().QueryContext(ctx,
		`SELECT peer_id, generation, digest, pushed_at FROM peer_control_backups ORDER BY peer_id`)
	if err != nil {
		return nil, fmt.Errorf("backupsync: reading beliefs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Belief
	for rows.Next() {
		var (
			bel      Belief
			pushedAt string
		)
		if err := rows.Scan(&bel.PeerID, &bel.Generation, &bel.Digest, &pushedAt); err != nil {
			return nil, fmt.Errorf("backupsync: reading a belief: %w", err)
		}
		if t, err := time.Parse(timestampFormat, pushedAt); err == nil {
			bel.PushedAt = t
		}
		out = append(out, bel)
	}
	return out, rows.Err()
}
