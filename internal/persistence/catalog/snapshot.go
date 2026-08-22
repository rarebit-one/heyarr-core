package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
)

// Catalog snapshots, from the controller's side (§52, §79, M4-13).
//
// This file builds the payload a Full Peer materialises and records what it
// issued in peer_snapshots. The peer's half — the separate, read-only snapshot
// database — is internal/peer/catalog, and the two are deliberately different
// packages against different files: §52's "the snapshot should not be treated
// as independently writable control state" is enforced by there being no
// writable path from one to the other (Invariant 5, ADR-0003).
//
// # Why the controller allocates the version
//
// Because it is the single writer, and monotonicity has to be allocated
// somewhere that a restart, a retry or a second peer cannot reset. A peer
// numbering its own snapshots would be one allocator per peer per process, and
// "monotonic" would quietly become "monotonic until something restarted".

// ErrNoPeerSnapshot is a peer the controller has never issued a snapshot to.
//
// Distinct from a snapshot containing nothing, for the reason M7 will care
// about: "the library is empty" and "this peer cannot help you" are different
// answers, and a design that returns a zero value for both eventually tells a
// user their library is empty during an outage.
var ErrNoPeerSnapshot = errors.New("catalog: this peer has no snapshot on record")

// SnapshotRecord is the control plane's account of one peer's snapshot.
type SnapshotRecord struct {
	PeerID        string
	ControllerID  string
	Version       int64
	GeneratedAt   time.Time
	Kind          string
	Watermark     time.Time
	RowCount      int64
	ContentDigest string
	UpdatedAt     time.Time
}

// Age reports how stale this peer's snapshot is at now.
func (r SnapshotRecord) Age(now time.Time) time.Duration { return now.Sub(r.GeneratedAt) }

// SnapshotRequest asks for a peer's next snapshot.
type SnapshotRequest struct {
	// PeerID is the peer the snapshot is for. It comes from the certificate on
	// the peer surface and never from a request body (ADR-0033).
	PeerID string
	// ControllerID is this controller's peer id, stamped into the payload so a
	// snapshot restored from another deployment (§51, §82) is recognisable as
	// somebody else's.
	ControllerID string
	// Holding is the version the peer says it already has. Zero means "none",
	// which is also what a peer that has lost its snapshot store reports — and
	// both correctly produce a full rebuild.
	Holding int64
	// Full forces the drift-correcting full rebuild even when an incremental
	// refresh would be legal. It is the same escape hatch M4-07 gives
	// inventory, and it exists because the cheapest way out of "the snapshot
	// disagrees with the catalogue and nobody knows why" is to stop being
	// clever.
	Full bool
}

// BuildSnapshot produces a peer's next snapshot and records that it did.
//
// # Full or incremental
//
// Incremental only when the peer holds exactly the version this controller
// last issued it. Anything else — a peer that holds nothing, a peer that holds
// a version this controller has no record of, a caller that asked for a full
// rebuild — takes the full path. That rule is deliberately conservative: an
// incremental refresh computed against a watermark the peer never actually
// applied would produce a snapshot that is confidently missing rows, which is
// the one failure mode worse than being slow.
//
// # Why the catalogue is read in one transaction
//
// A snapshot is a fact about a moment. Reading works in one transaction and
// assets in another would produce an artifact describing no moment at all —
// an asset pointing at an edition that had not been created when editions were
// read. One read transaction is what makes "this is the catalogue as of
// generated_at" true rather than approximately true.
func (c *Catalog) BuildSnapshot(ctx context.Context, req SnapshotRequest) (*peercatalog.Snapshot, error) {
	if req.PeerID == "" {
		return nil, errors.New("catalog: a snapshot must be built for a named peer")
	}
	if req.ControllerID == "" {
		return nil, errors.New("catalog: a snapshot must name the controller that built it")
	}

	previous, err := c.PeerSnapshot(ctx, req.PeerID)
	switch {
	case errors.Is(err, ErrNoPeerSnapshot):
		previous = SnapshotRecord{}
	case err != nil:
		return nil, err
	}

	incremental := !req.Full &&
		previous.Version > 0 &&
		req.Holding == previous.Version &&
		!previous.Watermark.IsZero()

	generatedAt := c.clock.Now().UTC()
	snap := &peercatalog.Snapshot{}
	var since time.Time
	kind := peercatalog.KindFull
	if incremental {
		kind = peercatalog.KindIncremental
		since = previous.Watermark
	}

	// One read transaction over the whole catalogue. Started on the reader
	// pool, which WAL lets proceed while a write is in flight, so a snapshot
	// build never blocks the control plane it is reading.
	tx, err := c.db.Reader().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog: beginning the snapshot read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := readSnapshotRows(ctx, tx, snap, since, incremental); err != nil {
		return nil, err
	}
	if incremental {
		if snap.IDs, err = readSnapshotIDs(ctx, tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return nil, fmt.Errorf("catalog: closing the snapshot read: %w", err)
	}

	version, err := c.recordSnapshot(ctx, req, snap, kind, generatedAt)
	if err != nil {
		return nil, err
	}
	snap.Meta = peercatalog.Meta{
		ControllerID: req.ControllerID,
		Version:      version,
		GeneratedAt:  generatedAt,
		Kind:         kind,
		// The next incremental refresh asks from the instant this one read,
		// and the source selects at-or-after it. Re-sending a row that changed
		// in the same nanosecond is harmless — every apply is an upsert —
		// whereas selecting strictly-after would drop it.
		Watermark: generatedAt,
	}
	return snap, nil
}

// recordSnapshot allocates the version and writes the control-plane record.
//
// Allocation and recording are the same transaction because they are the same
// fact. A version handed out and not recorded is a version this controller
// will hand out again, which is monotonicity failing in the one way that
// leaves no trace.
func (c *Catalog) recordSnapshot(
	ctx context.Context,
	req SnapshotRequest,
	snap *peercatalog.Snapshot,
	kind string,
	generatedAt time.Time,
) (int64, error) {
	var (
		version int64
		ev      events.Event
	)
	digest := snap.ContentDigest()
	rowCount := int64(snap.Rows())

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var current int64
		err := tx.QueryRowContext(ctx,
			`SELECT version FROM peer_snapshots WHERE peer_id = ?`, req.PeerID).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("catalog: reading the peer's snapshot version: %w", err)
		}
		version = current + 1

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO peer_snapshots
				(peer_id, controller_id, version, generated_at, kind, watermark,
				 row_count, content_digest, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (peer_id) DO UPDATE SET
				controller_id = excluded.controller_id, version = excluded.version,
				generated_at = excluded.generated_at, kind = excluded.kind,
				watermark = excluded.watermark, row_count = excluded.row_count,
				content_digest = excluded.content_digest, updated_at = excluded.updated_at`,
			req.PeerID, req.ControllerID, version, stampOf(generatedAt), kind, stampOf(generatedAt),
			rowCount, digest, stampOf(c.clock.Now())); err != nil {
			return fmt.Errorf("catalog: recording the peer snapshot: %w", err)
		}

		// One event per build (§76, Invariant 7). Not one per row — the
		// argument the events package makes about replication holds verbatim
		// here: a fact true of every item is state, and state belongs in a
		// table. The rows are already in the snapshot.
		ev, err = c.events.EmitTx(ctx, tx, events.TypeCatalogSnapshotBuilt, "peer", req.PeerID,
			map[string]any{
				"controller_id":  req.ControllerID,
				"version":        version,
				"kind":           kind,
				"generated_at":   stampOf(generatedAt),
				"rows":           rowCount,
				"content_digest": digest,
				// What the peer said it had. It is the input the full/
				// incremental decision turned on, and without it an operator
				// reading "full" cannot tell a forced rebuild from a peer that
				// had lost its store.
				"peer_holding": req.Holding,
			})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	c.events.Publish(ev)
	return version, nil
}

// PeerSnapshot reports what this controller last issued to a peer.
//
// [ErrNoPeerSnapshot] when it has never issued one — never a zero record.
func (c *Catalog) PeerSnapshot(ctx context.Context, peerID string) (SnapshotRecord, error) {
	var (
		r                             SnapshotRecord
		generated, watermark, updated string
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT peer_id, controller_id, version, generated_at, kind, watermark,
		       row_count, content_digest, updated_at
		FROM peer_snapshots WHERE peer_id = ?`, peerID).
		Scan(&r.PeerID, &r.ControllerID, &r.Version, &generated, &r.Kind, &watermark,
			&r.RowCount, &r.ContentDigest, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SnapshotRecord{}, fmt.Errorf("%w: %s", ErrNoPeerSnapshot, peerID)
	case err != nil:
		return SnapshotRecord{}, fmt.Errorf("catalog: reading the peer snapshot record: %w", err)
	}
	if r.GeneratedAt, err = parseSnapshotStamp(generated); err != nil {
		return SnapshotRecord{}, err
	}
	if r.Watermark, err = parseSnapshotStamp(watermark); err != nil {
		return SnapshotRecord{}, err
	}
	if r.UpdatedAt, err = parseSnapshotStamp(updated); err != nil {
		return SnapshotRecord{}, err
	}
	return r, nil
}

// AllPeerSnapshots reports every peer's snapshot record, keyed by peer id.
//
// One query rather than one per peer: the collection view answers "which peers
// are stale?", and a per-peer lookup would make the cost of asking scale with
// the number of peers at exactly the moment an operator is scanning them all.
// Peers this controller has never issued a snapshot to are simply absent from
// the map — the caller renders that as "none", which is not the same answer as
// a snapshot of an empty library.
func (c *Catalog) AllPeerSnapshots(ctx context.Context) (map[string]SnapshotRecord, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT peer_id, controller_id, version, generated_at, kind, watermark,
		       row_count, content_digest, updated_at
		FROM peer_snapshots`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading peer snapshot records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]SnapshotRecord{}
	for rows.Next() {
		var (
			r                             SnapshotRecord
			generated, watermark, updated string
		)
		if err := rows.Scan(&r.PeerID, &r.ControllerID, &r.Version, &generated, &r.Kind,
			&watermark, &r.RowCount, &r.ContentDigest, &updated); err != nil {
			return nil, fmt.Errorf("catalog: reading peer snapshot records: %w", err)
		}
		if r.GeneratedAt, err = parseSnapshotStamp(generated); err != nil {
			return nil, err
		}
		if r.Watermark, err = parseSnapshotStamp(watermark); err != nil {
			return nil, err
		}
		if r.UpdatedAt, err = parseSnapshotStamp(updated); err != nil {
			return nil, err
		}
		out[r.PeerID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading peer snapshot records: %w", err)
	}
	return out, nil
}

func stampOf(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseSnapshotStamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("catalog: %q is not a snapshot timestamp: %w", s, err)
	}
	return t.UTC(), nil
}
