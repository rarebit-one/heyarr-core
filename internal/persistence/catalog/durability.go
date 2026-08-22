package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// The control plane's half of ADR-0018's placement precondition (M4-12).
//
// Everything here answers one of two questions and never both: what does this
// catalog BELIEVE about where a blob is (Peers, Replicas), and what has been
// ESTABLISHED about it (RecordDurability, DurabilityEvidence). Verifying the
// first into the second happens over the peer fabric, in
// internal/peer/durability, because a claim checked by the same table that made
// it is not checked.

// Peers lists every peer other than this node.
//
// The self row is excluded in the SQL rather than by the caller. "Is this blob
// somewhere else" answered by a row describing this machine is the precise
// failure ADR-0018 names, and leaving that filter to callers is leaving it to
// be forgotten once.
func (c *Catalog) Peers(ctx context.Context) ([]integrity.Peer, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT id, name, coalesce(endpoint, ''), public_key, health, last_seen_at
		FROM peers WHERE is_self = 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing the other peers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []integrity.Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing the other peers: %w", err)
	}
	return out, nil
}

// Replicas is what this catalog believes other peers hold of one blob.
//
// It is an inner join to `peers` with is_self = 0, so the answer is only ever
// about elsewhere, and it carries reported_at (00023) because freshness is the
// difference between a fact and a fact about the past. NULL reported_at comes
// back as the zero time, which [integrity.Replica.Fresh] treats as never
// confirmed rather than as confirmed long ago.
func (c *Catalog) Replicas(ctx context.Context, h hashing.Hash) ([]integrity.Replica, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT p.id, p.name, coalesce(p.endpoint, ''), p.public_key, p.health, p.last_seen_at,
		       r.state, r.bytes_present, r.verified_at, r.reported_at
		FROM replicas r JOIN peers p ON p.id = r.peer_id
		WHERE r.blob_hash = ? AND p.is_self = 0
		ORDER BY p.id`, h.String())
	if err != nil {
		return nil, fmt.Errorf("catalog: reading who else holds %s: %w", h, err)
	}
	defer func() { _ = rows.Close() }()

	var out []integrity.Replica
	for rows.Next() {
		var (
			r          integrity.Replica
			endpoint   string
			health     string
			lastSeen   sql.NullString
			verifiedAt sql.NullString
			reportedAt sql.NullString
			publicKey  []byte
		)
		if err := rows.Scan(&r.Peer.PeerID, &r.Peer.Name, &endpoint, &publicKey, &health, &lastSeen,
			&r.State, &r.BytesPresent, &verifiedAt, &reportedAt); err != nil {
			return nil, fmt.Errorf("catalog: reading who else holds %s: %w", h, err)
		}
		r.Peer.Endpoint, r.Peer.PublicKey, r.Peer.Health = endpoint, publicKey, health
		r.Peer.LastSeenAt = nullStamp(lastSeen)
		r.VerifiedAt, r.ReportedAt = nullStamp(verifiedAt), nullStamp(reportedAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading who else holds %s: %w", h, err)
	}
	return out, nil
}

// MarkReplicaMissing corrects a row a peer contradicted.
//
// The WHERE clause excludes rows that already say missing, which is what keeps
// this idempotent (invariant 9) and stops every sweep after a loss from
// emitting a fresh replica.missing as though the loss had just happened — the
// same rule internal/persistence/catalog/inventory.go applies to absence in a
// full report.
//
// reported_at is deliberately NOT advanced. The peer did not confirm anything;
// it was caught out. Stamping it would make a row that was just proved wrong
// look like the freshest row in the table.
func (c *Catalog) MarkReplicaMissing(ctx context.Context, h hashing.Hash, peerID string, at time.Time) error {
	hash := h.String()
	stamp := at.UTC().Format(timestampFormat)

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = pending[:0]
		var previous string
		err := tx.QueryRowContext(ctx,
			`SELECT state FROM replicas WHERE blob_hash = ? AND peer_id = ?`, hash, peerID).Scan(&previous)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("catalog: reading peer %s's replica of %s: %w", peerID, hash, err)
		case previous == "missing":
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE replicas SET state = 'missing', bytes_present = 0, updated_at = ?
			WHERE blob_hash = ? AND peer_id = ?`, stamp, hash, peerID); err != nil {
			return fmt.Errorf("catalog: correcting peer %s's claim to hold %s: %w", peerID, hash, err)
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaMissing, "blob", hash, map[string]any{
			"peer_id":       peerID,
			"previous":      previous,
			"bytes_present": 0,
			// The source is what tells a subscriber how this was learned. Not
			// an inventory report and not a local verification: garbage
			// collection asked the peer directly, before deleting the last
			// local copy, and the peer's answer contradicted the row.
			"source": "durability_check",
		})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// RecordDurability writes down why a blob was believed durable elsewhere.
//
// It is called BEFORE the reclaim that relies on it — see migration 00028 and
// the comment on Reclaim. The row has no foreign key to `blobs`, so it survives
// the delete it authorised; that survival is the entire point, and asserting it
// is one of this issue's acceptance conditions.
func (c *Catalog) RecordDurability(ctx context.Context, e integrity.Evidence) error {
	if e.Basis != integrity.BasisVerifiedRemote && e.Basis != integrity.BasisSolePeer {
		// The CHECK constraint would catch this, and its message would name
		// neither the blob nor the caller. A basis this package does not
		// recognise is a new way of deciding it is safe to delete, and it
		// should be refused where it can be explained.
		return fmt.Errorf("catalog: refusing to record durability evidence for %s on the basis %q, "+
			"which is not a basis this system establishes anything on", e.BlobHash, e.Basis)
	}
	recorded := e.RecordedAt
	if recorded.IsZero() {
		recorded = c.clock.Now()
	}
	_, err := c.db.Writer().ExecContext(ctx, `
		INSERT INTO durability_evidence
			(id, blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at,
			 size, detail, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.Must(uuid.NewV7()).String(), e.BlobHash.String(), e.PeerID, e.PeerName, e.Endpoint,
		e.Basis, stampOrNull(e.ReportedAt), stampOrNull(e.VerifiedAt), e.Size, e.Detail,
		recorded.UTC().Format(timestampFormat))
	if err != nil {
		return fmt.Errorf("catalog: recording why %s was believed durable elsewhere: %w", e.BlobHash, err)
	}
	return nil
}

// DurabilityEvidence reads the evidence back, newest first.
//
// The blob it names has very probably been reclaimed by the time anyone asks,
// which is exactly when the question "on what grounds?" gets asked.
func (c *Catalog) DurabilityEvidence(ctx context.Context, h hashing.Hash) ([]integrity.Evidence, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at,
		       size, detail, recorded_at
		FROM durability_evidence WHERE blob_hash = ? ORDER BY id DESC`, h.String())
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the durability evidence for %s: %w", h, err)
	}
	defer func() { _ = rows.Close() }()

	var out []integrity.Evidence
	for rows.Next() {
		var (
			e                                  integrity.Evidence
			hash                               string
			reportedAt, verifiedAt, recordedAt sql.NullString
		)
		if err := rows.Scan(&hash, &e.PeerID, &e.PeerName, &e.Endpoint, &e.Basis,
			&reportedAt, &verifiedAt, &e.Size, &e.Detail, &recordedAt); err != nil {
			return nil, fmt.Errorf("catalog: reading the durability evidence for %s: %w", h, err)
		}
		parsed, err := hashing.Parse(hash)
		if err != nil {
			return nil, fmt.Errorf("catalog: durability evidence names %q, which is not a digest: %w", hash, err)
		}
		e.BlobHash = parsed
		e.ReportedAt = nullStamp(reportedAt)
		e.VerifiedAt = nullStamp(verifiedAt)
		e.RecordedAt = nullStamp(recordedAt)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading the durability evidence for %s: %w", h, err)
	}
	return out, nil
}

// hasDurabilityEvidence is the defence-in-depth check at the reclaim seam.
//
// It is the same shape as assets.blob_hash being ON DELETE RESTRICT, and it is
// there for the same reason: every other bug in this system costs a re-run, and
// this one costs the operator their content, so it is worth being told twice.
// The collector evaluates the placement precondition and this refuses the
// delete if no evidence of that evaluation exists — so a collector that lost
// its precondition to a refactor still cannot unlink a tracked blob.
func hasDurabilityEvidence(ctx context.Context, tx *sql.Tx, hash string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM durability_evidence WHERE blob_hash = ? LIMIT 1`, hash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: looking for the durability evidence for %s: %w", hash, err)
	}
	return true, nil
}

func scanPeer(row interface{ Scan(...any) error }) (integrity.Peer, error) {
	var (
		p         integrity.Peer
		endpoint  string
		health    string
		lastSeen  sql.NullString
		publicKey []byte
	)
	if err := row.Scan(&p.PeerID, &p.Name, &endpoint, &publicKey, &health, &lastSeen); err != nil {
		return integrity.Peer{}, fmt.Errorf("catalog: reading a peer: %w", err)
	}
	p.Endpoint, p.PublicKey, p.Health = endpoint, publicKey, health
	p.LastSeenAt = nullStamp(lastSeen)
	return p, nil
}

// nullStamp renders a nullable timestamp column as a time, with NULL and an
// unparseable value both becoming the zero time.
//
// The zero time is load-bearing rather than a fallback: reported_at IS NULL
// means "no peer has ever confirmed this row", and Fresh refuses a zero time
// outright. A column that failed to parse must land in the same place, because
// the alternative is a garbled timestamp reading as a recent one.
func nullStamp(v sql.NullString) time.Time {
	if !v.Valid || v.String == "" {
		return time.Time{}
	}
	ts, err := time.Parse(timestampFormat, v.String)
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

// stampOrNull renders the zero time as SQL NULL, so that "never" stays
// distinguishable from "in the year 1".
func stampOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timestampFormat)
}
