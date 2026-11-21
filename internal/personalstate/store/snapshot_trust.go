package store

// snapshot_trust.go is the peer's half of snapshot trust (#681, the ADR-0101
// amendment): the pinned compaction base, the cold-start choice that prefers it,
// the durable per-peer acknowledgements, and the compaction they prove safe.
// Everything here is keyless causal metadata. The peer cannot tell a genuine
// snapshot frontier from a relabelled one; a device checks the authenticated
// envelope, and the peer only ever chooses by what the change DAG proves.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// The errors snapshot trust refuses with.
var (
	// ErrUnknownSnapshot is a snapshot id this peer does not hold for the space.
	ErrUnknownSnapshot = errors.New("personalstate/store: no such snapshot in that space")
	// ErrBaseNotCovering is a pin whose snapshot does not provably cover the
	// space's current base: pinning it would let a cold start skip what the
	// current base folded and the log may no longer hold.
	ErrBaseNotCovering = errors.New("personalstate/store: the snapshot does not cover the space's pinned base")
)

// SnapshotByID returns one snapshot of a space by its content-addressed id.
func (s *Store) SnapshotByID(ctx context.Context, spaceID, snapshotID string) (protocol.EncryptedSnapshot, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return protocol.EncryptedSnapshot{}, err
	}
	snap, ok, err := scanSnapshot(s.reader.QueryRowContext(ctx,
		`SELECT snapshot_id, space_id, frontier, ciphertext
		 FROM encrypted_snapshots WHERE space_id = ? AND snapshot_id = ?`, spaceID, snapshotID))
	if err != nil {
		return protocol.EncryptedSnapshot{}, err
	}
	if !ok {
		return protocol.EncryptedSnapshot{}, fmt.Errorf("%w: %s", ErrUnknownSnapshot, snapshotID)
	}
	return snap, nil
}

// Base returns the space's pinned compaction base, and whether it has one.
func (s *Store) Base(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return protocol.EncryptedSnapshot{}, false, err
	}
	return scanSnapshot(s.reader.QueryRowContext(ctx,
		`SELECT n.snapshot_id, n.space_id, n.frontier, n.ciphertext
		 FROM snapshot_bases b JOIN encrypted_snapshots n ON n.snapshot_id = b.snapshot_id
		 WHERE b.space_id = ?`, spaceID))
}

// ColdStartSnapshot is the snapshot a device with no local state should start
// from (#681): the newest snapshot whose frontier covers the pinned base, else
// the base itself. With no base, nothing was ever compacted against a pin, and
// it is the newest snapshot, as LatestSnapshotFor has always answered.
//
// A snapshot pushed by an offline member can predate, or run concurrent with,
// the base a compaction ran against. A device that opened it would miss the
// compacted range, so it is passed over whatever its arrival time. The choice
// rests on the change DAG alone, so the device still verifies the envelope and
// falls back to the base (GET …/snapshot/base) when this one fails.
func (s *Store) ColdStartSnapshot(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error) {
	base, ok, err := s.Base(ctx, spaceID)
	if err != nil || !ok {
		if err == nil {
			return s.LatestSnapshotFor(ctx, spaceID)
		}
		return protocol.EncryptedSnapshot{}, false, err
	}
	changes, err := s.changeGraph(ctx, spaceID)
	if err != nil {
		return protocol.EncryptedSnapshot{}, false, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT snapshot_id, frontier FROM encrypted_snapshots
		 WHERE space_id = ? ORDER BY created_at DESC, rowid DESC`, spaceID)
	if err != nil {
		return protocol.EncryptedSnapshot{}, false, fmt.Errorf("personalstate/store: listing snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, frontier string
		if err := rows.Scan(&id, &frontier); err != nil {
			return protocol.EncryptedSnapshot{}, false, fmt.Errorf("personalstate/store: reading snapshot: %w", err)
		}
		if id == base.SnapshotID {
			continue // the fallback below
		}
		if protocol.Covers(changes, splitFrontier(frontier), base.Frontier) {
			_ = rows.Close()
			snap, err := s.SnapshotByID(ctx, spaceID, id)
			return snap, err == nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return protocol.EncryptedSnapshot{}, false, fmt.Errorf("personalstate/store: listing snapshots: %w", err)
	}
	return base, true, nil
}

// PinBase makes a held snapshot the space's compaction base. It is idempotent:
// re-pinning the base is a no-op that reports pinned false. A snapshot that does
// not provably cover the current base is refused with ErrBaseNotCovering, so the
// base only ever moves forward. A compaction pins the snapshot it names, and a
// replicating sibling pins the base it pushes.
func (s *Store) PinBase(ctx context.Context, spaceID, snapshotID string) (pinned bool, err error) {
	snap, err := s.SnapshotByID(ctx, spaceID, snapshotID)
	if err != nil {
		return false, err
	}
	changes, err := s.changeGraph(ctx, spaceID)
	if err != nil {
		return false, err
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevID, prevFrontier string
	err = tx.QueryRowContext(ctx,
		`SELECT b.snapshot_id, n.frontier FROM snapshot_bases b
		 JOIN encrypted_snapshots n ON n.snapshot_id = b.snapshot_id WHERE b.space_id = ?`, spaceID).
		Scan(&prevID, &prevFrontier)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("personalstate/store: reading the base: %w", err)
	case prevID == snapshotID:
		return false, tx.Commit()
	case !protocol.Covers(changes, snap.Frontier, splitFrontier(prevFrontier)):
		return false, fmt.Errorf("%w: %s does not cover %s", ErrBaseNotCovering, snapshotID, prevID)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshot_bases (space_id, snapshot_id, pinned_at) VALUES (?, ?, ?)
		 ON CONFLICT (space_id) DO UPDATE SET snapshot_id = excluded.snapshot_id, pinned_at = excluded.pinned_at`,
		spaceID, snapshotID, s.clock.Now().UTC().Format(timeFormat)); err != nil {
		return false, fmt.Errorf("personalstate/store: pinning the base: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSnapshotPinned, "encrypted_space", spaceID,
		map[string]any{"snapshot_id": snapshotID, "previous": prevID})
	if err != nil {
		return false, fmt.Errorf("personalstate/store: recording the pin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return true, nil
}

// OfferedHeads is what this peer offers a replicating sibling as its heads: the
// heads of the changes it holds, plus the frontier of its pinned base. A peer
// that compacted holds the base in place of the rows it dropped, so a sibling
// computing what it is missing must count the base's causal history as held, or
// every pass would resend the compacted range and undo the compaction (#682).
func (s *Store) OfferedHeads(ctx context.Context, spaceID string) ([]string, error) {
	heads, err := s.HeadsFor(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	base, ok, err := s.Base(ctx, spaceID)
	if err != nil || !ok {
		return heads, err
	}
	out := slices.Clone(heads)
	for _, f := range base.Frontier {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out, nil
}

// HeldChanges answers which of ids this peer holds as change rows of the space,
// in the order asked. It is what a sibling's audit asks before it records an
// acknowledgement or resends a change (#681, #682): the target confirms each id
// itself, rather than the source inferring it from the target's heads.
func (s *Store) HeldChanges(ctx context.Context, spaceID string, ids []string) ([]string, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(ids))
	const batch = 500 // well under SQLite's bound on host parameters
	for start := 0; start < len(ids); start += batch {
		chunk := ids[start:min(start+batch, len(ids))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, spaceID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.reader.QueryContext(ctx,
			`SELECT change_id FROM encrypted_changes WHERE space_id = ? AND change_id IN (?`+
				strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("personalstate/store: confirming held changes: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("personalstate/store: confirming held changes: %w", err)
			}
			held[id] = true
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("personalstate/store: confirming held changes: %w", err)
		}
	}
	out := make([]string, 0, len(held))
	for _, id := range ids {
		if held[id] {
			out = append(out, id)
			delete(held, id) // a repeated id is answered once
		}
	}
	return out, nil
}

// A PeerAck is what a trusted Full Peer confirmed holding of a space at its
// last audited reconcile: every change in the causal history of Frontier, over
// this node's log, and the base it had pinned ("" for none).
type PeerAck struct {
	PeerID   string
	Frontier []string
	Base     string
}

// RecordPeerAck durably records a peer's confirmed holding of a space, replacing
// its previous record. The caller (the replication audit) writes it only after
// the peer confirmed every id itself. An unchanged record is not rewritten, so a
// converged fleet emits nothing.
func (s *Store) RecordPeerAck(ctx context.Context, spaceID, peerID string, frontier []string, base string) error {
	if peerID == "" {
		return errors.New("personalstate/store: an acknowledgement needs a peer id")
	}
	canon := slices.Clone(frontier)
	slices.Sort(canon)
	canon = slices.Compact(canon)
	joined := strings.Join(canon, ",")

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevFrontier, prevBase string
	err = tx.QueryRowContext(ctx,
		`SELECT frontier, base_snapshot_id FROM state_peer_acks WHERE space_id = ? AND peer_id = ?`,
		spaceID, peerID).Scan(&prevFrontier, &prevBase)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("personalstate/store: reading an acknowledgement: %w", err)
	case prevFrontier == joined && prevBase == base:
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO state_peer_acks (space_id, peer_id, frontier, base_snapshot_id, acked_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (space_id, peer_id) DO UPDATE SET
		   frontier = excluded.frontier, base_snapshot_id = excluded.base_snapshot_id, acked_at = excluded.acked_at`,
		spaceID, peerID, joined, base, s.clock.Now().UTC().Format(timeFormat)); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return fmt.Errorf("%w: %s", ErrUnknownSpace, spaceID)
		}
		return fmt.Errorf("personalstate/store: recording an acknowledgement: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeStatePeerAcked, "encrypted_space", spaceID,
		map[string]any{"peer_id": peerID, "heads": len(canon), "base": base})
	if err != nil {
		return fmt.Errorf("personalstate/store: recording an acknowledgement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return nil
}

// PeerAcks returns every acknowledgement recorded for a space, by peer id.
func (s *Store) PeerAcks(ctx context.Context, spaceID string) (map[string]PeerAck, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT peer_id, frontier, base_snapshot_id FROM state_peer_acks WHERE space_id = ?`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing acknowledgements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]PeerAck{}
	for rows.Next() {
		var a PeerAck
		var frontier string
		if err := rows.Scan(&a.PeerID, &frontier, &a.Base); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading an acknowledgement: %w", err)
		}
		a.Frontier = splitFrontier(frontier)
		out[a.PeerID] = a
	}
	return out, rows.Err()
}

// A CompactResult is what one compaction did: the base it pinned, how many
// changes it dropped, and the trusted Full Peers it is still waiting for.
type CompactResult struct {
	Base    string
	Pinned  bool
	Dropped int
	// Waiting lists the peers whose acknowledgement does not yet name this base.
	// While it is non-empty nothing is dropped; the next audited reconcile
	// replicates the pin and records their acks, and running again drops.
	Waiting []string
}

// CompactChanges compacts a space against the snapshot it names (#681, §44). It
// pins that snapshot as the space's compaction base, then drops a change only if
// BOTH:
//
//  1. the pinned snapshot subsumes it, so the change is recoverable from the
//     base that every device now prefers to cold-start from; AND
//  2. every trusted Full Peer in fullPeers has a durable acknowledgement that
//     names this same base and whose acked causal history includes the change,
//     so no peer still needs the raw change and every peer holds the base.
//
// 🔴 The double condition is the whole safety of compaction. A frontier supplied
// by the caller is no longer accepted: the proof is what each peer confirmed
// holding, recorded by this node's audited reconcile. With no trusted Full
// Peers, condition 2 is vacuous: this node's log is the only copy, and the base
// keeps its state recoverable.
func (s *Store) CompactChanges(ctx context.Context, spaceID, snapshotID string, fullPeers []string) (CompactResult, error) {
	pinned, err := s.PinBase(ctx, spaceID, snapshotID)
	if err != nil {
		return CompactResult{}, err
	}
	res := CompactResult{Base: snapshotID, Pinned: pinned}
	snap, err := s.SnapshotByID(ctx, spaceID, snapshotID)
	if err != nil {
		return res, err
	}
	acks, err := s.PeerAcks(ctx, spaceID)
	if err != nil {
		return res, err
	}
	for _, p := range fullPeers {
		if a, ok := acks[p]; !ok || a.Base != snapshotID {
			res.Waiting = append(res.Waiting, p)
		}
	}
	if len(res.Waiting) > 0 {
		slices.Sort(res.Waiting)
		return res, nil
	}
	changes, err := s.changeGraph(ctx, spaceID)
	if err != nil {
		return res, err
	}
	subsumed := protocol.CausalHistory(changes, snap.Frontier)
	acked := make([]map[string]bool, 0, len(fullPeers))
	for _, p := range fullPeers {
		acked = append(acked, protocol.CausalHistory(changes, acks[p].Frontier))
	}
	var droppable []string
	for _, c := range changes {
		if !subsumed[c.ChangeID] {
			continue
		}
		everyPeer := true
		for _, a := range acked {
			if !a[c.ChangeID] {
				everyPeer = false
				break
			}
		}
		if everyPeer {
			droppable = append(droppable, c.ChangeID)
		}
	}
	if len(droppable) == 0 {
		return res, nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The base is re-checked inside the transaction, so a newer pin landing since
	// the read above is not compacted against an older snapshot's proof.
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM snapshot_bases WHERE space_id = ?`, spaceID).Scan(&current); err != nil {
		return res, fmt.Errorf("personalstate/store: re-reading the base: %w", err)
	}
	if current != snapshotID {
		return res, fmt.Errorf("%w: the base moved to %s while compacting", ErrBaseNotCovering, current)
	}
	dropped := 0
	for _, id := range droppable {
		r, err := tx.ExecContext(ctx, `DELETE FROM encrypted_changes WHERE change_id = ? AND space_id = ?`, id, spaceID)
		if err != nil {
			return res, fmt.Errorf("personalstate/store: compacting change: %w", err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			dropped++
		}
	}
	if dropped == 0 {
		return res, tx.Commit()
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeChangesCompacted, "encrypted_space", spaceID,
		map[string]any{"snapshot_id": snapshotID, "dropped": dropped, "peers": len(fullPeers)})
	if err != nil {
		return res, fmt.Errorf("personalstate/store: recording compaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	res.Dropped = dropped
	return res, nil
}

// changeGraph is a space's change log without its ciphertext: the ids and
// parents a keyless causal walk needs, which is all this file ever reads.
func (s *Store) changeGraph(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT change_id, parents FROM encrypted_changes WHERE space_id = ? ORDER BY seq`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: reading the change graph: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []protocol.EncryptedChange
	for rows.Next() {
		var c protocol.EncryptedChange
		var parents string
		if err := rows.Scan(&c.ChangeID, &parents); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading the change graph: %w", err)
		}
		c.Parents = splitFrontier(parents)
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanSnapshot(row *sql.Row) (protocol.EncryptedSnapshot, bool, error) {
	var snap protocol.EncryptedSnapshot
	var frontier string
	err := row.Scan(&snap.SnapshotID, &snap.SpaceID, &frontier, &snap.Ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.EncryptedSnapshot{}, false, nil
	}
	if err != nil {
		return protocol.EncryptedSnapshot{}, false, fmt.Errorf("personalstate/store: reading snapshot: %w", err)
	}
	snap.Frontier = splitFrontier(frontier)
	return snap, true, nil
}

func splitFrontier(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}
