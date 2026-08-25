package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// One blob transfer, as the control plane records it (§21, ADR-0030, M4-09).
//
// # `pending` finally has a writer
//
// `replicas.state` has carried 'pending' since migration 00002 and nothing has
// ever written it. This file is what it was reserved for, and the reason it
// matters is the third constraint of M4-09: a blob that arrived half-way is not
// a replica, and a `present` row written before verification finishes is a
// claim that the library is safer than it is. Garbage collection reads that
// table before unlinking what it believes is a surplus copy (ADR-0018), so a
// premature `present` is not a cosmetic inaccuracy — it is the input to a
// decision that deletes bytes.
//
// So the row moves `pending` → `present` and never the other way round without
// evidence: `present` is written by exactly one function here, after the bytes
// are on this node's disk and have been hashed by this node.
//
// # Why the failure states are the ones a peer could have reported
//
// A failed transfer leaves `missing` and a refused one leaves `corrupt`, which
// are the same two states an inventory report can produce (M4-07). That is
// deliberate: "this peer does not hold these bytes" and "this peer holds bytes
// that are not what they claim to be, and they are quarantined" are facts about
// a disk, and a fact should not be spelled differently depending on which
// mechanism noticed it. Reconciliation counts neither as held, so the next
// cycle offers the gap again — which is the retry.

// BlobSources is every peer this node believes holds these bytes and can be
// dialled for them, ranked best-first.
//
// # It reads `replicas`, and `replicas` is a belief
//
// The rows are what peers have REPORTED (M4-07), which is a snapshot and may be
// stale by the time a transfer runs. That is why the puller treats a 404 as
// "try the next source" rather than as an error: an inventory going out of date
// between a report and a pull is ordinary, and the list below is a list of
// candidates rather than of guarantees.
//
// # The self peer is excluded, and so is a peer with nothing to pin
//
// Self, because a node pulling from itself over TLS is a bug wearing a network
// hop. And a peer with no public key or no endpoint is dropped by
// [replication.RankSources] rather than attempted: membership is the only trust
// root in the inter-peer path (ADR-0012), so a candidate with no pinned key is
// a candidate that would have to be trusted on first use.
//
// Note what makes revocation work here without a line of its own: `replicas`
// references `peers` with ON DELETE CASCADE, and revocation is the deletion of
// the membership record (ADR-0012). Removing a peer therefore removes it from
// this answer, and there is no separate "is it still a member" check to forget.
func (c *Catalog) BlobSources(ctx context.Context, blobHash string) ([]replication.Source, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT p.id, p.name, coalesce(p.endpoint, ''), p.public_key, coalesce(p.health, 'unknown')
		  FROM replicas r
		  JOIN peers p ON p.id = r.peer_id
		 WHERE r.blob_hash = ? AND r.state = 'present' AND p.id <> ?
		 ORDER BY p.id`, blobHash, self)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the sources for %s: %w", blobHash, err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []replication.Source
	for rows.Next() {
		var (
			src replication.Source
			key []byte
		)
		if err := rows.Scan(&src.PeerID, &src.Name, &src.Endpoint, &key, &src.Health); err != nil {
			return nil, fmt.Errorf("catalog: reading the sources for %s: %w", blobHash, err)
		}
		src.PublicKey = key
		candidates = append(candidates, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading the sources for %s: %w", blobHash, err)
	}
	// The ranking is a pure function in the domain, and the query does not do
	// it: a decision expressed as an ORDER BY is a decision that cannot be
	// asserted without a database (ADR-0006).
	return replication.RankSources(candidates), nil
}

// BlobTransfer names one transfer for the record: which bytes, from where, to
// where.
type BlobTransfer struct {
	BlobHash string
	// SourcePeerID may be empty on a failure that happened before a source was
	// chosen — no candidate held the bytes, or none could be authenticated.
	SourcePeerID string
	// DestinationPeerID is the peer that must end up holding the bytes, which
	// on this code path is always this node: a destination pulls its own bytes
	// (ADR-0030).
	DestinationPeerID string
	// Bytes is how many arrived and verified. Zero on every outcome but
	// success.
	Bytes int64
	// Sources is how many pieces each peer contributed, for a transfer that
	// had more than one.
	//
	// # Why this exists rather than SourcePeerID naming the biggest contributor
	//
	// A piece transfer has no single source. Writing one peer's id into
	// SourcePeerID would be a claim that peer served the blob, which is false,
	// and it is false in a way nothing downstream could catch: the value is
	// well-formed, it names a peer that really did contribute, and every
	// consumer would believe it. The M5 accounting assertion — that a source's
	// own record of what it served agrees with the destination's — would then
	// compare one peer's bytes against four peers' worth and fail for a reason
	// that had nothing to do with the fault.
	//
	// So SourcePeerID stays EMPTY for these, which is a state it already has a
	// meaning for ("no single source was chosen"), and the breakdown goes here.
	// Empty for an ordinary single-source transfer.
	//
	// # Why a payload field and not a column
	//
	// Because SourcePeerID is not a column either. It is carried in the
	// transfer event, and `replicas` records where replicas ARE rather than
	// where they came from — so per-source attribution needs no migration, and
	// inventing a table for it would add a schema that nothing yet reads.
	Sources map[string]int

	// Reason explains a failure in one phrase, for the event payload. Empty on
	// success.
	Reason string
}

// BeginBlobTransfer marks the replica `pending` and emits the started
// transition.
//
// `pending` is a claim about work, not about bytes: it says a transfer is in
// flight, and everything that asks "is this blob safe here" — reconciliation,
// read routing, garbage collection — counts only `present`. That is what makes
// writing it early correct and writing `present` early a bug.
//
// It refuses to overwrite a `present` row. A transfer against a blob this node
// already holds is a re-run (invariant 9), and downgrading a verified replica
// to `pending` because a duplicate job was claimed would make the fabric
// briefly believe it had lost a copy — during which garbage collection
// elsewhere could act on it.
func (c *Catalog) BeginBlobTransfer(ctx context.Context, t BlobTransfer) error {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			VALUES (?, ?, 'pending', 0, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'pending', bytes_present = 0, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present'`,
			t.BlobHash, t.DestinationPeerID, now); err != nil {
			return fmt.Errorf("catalog: marking %s pending on peer %s: %w",
				t.BlobHash, t.DestinationPeerID, err)
		}
		var err error
		ev, err = c.emitTransferChanged(ctx, tx, replication.TransferStarted, t)
		return err
	})
	if err != nil {
		return err
	}
	c.events.Publish(ev)
	return nil
}

// RecordBlobTransferred marks the replica `present` and emits the succeeded
// transition.
//
// This is the only function in this package that writes `present` for bytes
// that arrived over the network, and it runs after they are on this node's disk
// and have been hashed by this node. verified_at is stamped because that is
// literally true: the destination hashed every byte as it landed (§21). Nothing
// else in the system has to take a source's word for anything.
//
// # A row that already says `present` is left alone, and emits nothing
//
// The write is conditional and the event follows the write, which is what makes
// a re-run free rather than merely harmless (invariant 9). A handler that ran
// again over a blob this node already holds has established no new fact: the
// row said `present` before it started and says `present` after, and announcing
// a transition that did not happen would turn every retry into event noise.
// It is the same rule inventory reconciliation follows for an unchanged report,
// and it is why the emit is inside the `changed` branch rather than beside it.
func (c *Catalog) RecordBlobTransferred(ctx context.Context, t BlobTransfer) error {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var (
		ev      events.Event
		changed bool
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		changed = false
		res, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				verified_at = excluded.verified_at, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present' OR replicas.bytes_present <> excluded.bytes_present`,
			t.BlobHash, t.DestinationPeerID, t.Bytes, now, now)
		if err != nil {
			return fmt.Errorf("catalog: recording the transferred replica of %s on peer %s: %w",
				t.BlobHash, t.DestinationPeerID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("catalog: recording the transferred replica of %s on peer %s: %w",
				t.BlobHash, t.DestinationPeerID, err)
		}
		if n == 0 {
			return nil
		}
		changed = true
		ev, err = c.emitTransferChanged(ctx, tx, replication.TransferSucceeded, t)
		return err
	})
	if err != nil {
		return err
	}
	if changed {
		c.events.Publish(ev)
	}
	return nil
}

// RecordBlobTransferFailed records a transfer that did not produce a replica,
// and emits the failed transition.
//
// state is 'missing' for a transfer that never delivered whole bytes, and
// 'corrupt' for one whose bytes arrived and did not verify — the second is not
// an absence, it is evidence sitting in quarantine/ (ADR-0018), and an operator
// looking at a `corrupt` row has somewhere to go.
//
// A `present` row is never overwritten here either. The blob may already be on
// this node from an ingest or an earlier transfer, and a failure to fetch a
// second copy of something we hold is not a reason to say we have lost it.
func (c *Catalog) RecordBlobTransferFailed(ctx context.Context, t BlobTransfer, state string) error {
	if state != "missing" && state != "corrupt" {
		return fmt.Errorf("catalog: %q is not a state a failed transfer may record", state)
	}
	now := c.clock.Now().UTC().Format(timestampFormat)
	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			VALUES (?, ?, ?, 0, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = excluded.state, bytes_present = 0, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present'`,
			t.BlobHash, t.DestinationPeerID, state, now); err != nil {
			return fmt.Errorf("catalog: recording the failed transfer of %s to peer %s: %w",
				t.BlobHash, t.DestinationPeerID, err)
		}
		var err error
		ev, err = c.emitTransferChanged(ctx, tx, replication.TransferFailed, t)
		return err
	})
	if err != nil {
		return err
	}
	c.events.Publish(ev)
	return nil
}

// emitTransferChanged emits replication.transfer_changed.
//
// ONE event type for the whole lifecycle, with the transition in the payload.
// events.go argues the general case — N types are N places to forget to emit —
// and the peer plane adds a second reason: this is the only per-blob event
// replication is allowed, and it is allowed because a transfer is discrete work
// on a queue an operator throttles, bounded by the transfer rate rather than by
// the size of the library. A vocabulary of started/succeeded/failed types would
// have made "add one more edge" the obvious next move.
//
// The subject is the blob and both peers are in the payload, matching how every
// other replica event in this package is shaped: a subscriber following one
// blob across the fabric follows one subject.
func (c *Catalog) emitTransferChanged(
	ctx context.Context, tx *sql.Tx, transition string, t BlobTransfer,
) (events.Event, error) {
	payload := map[string]any{
		"transition":          transition,
		"blob_hash":           t.BlobHash,
		"source_peer_id":      t.SourcePeerID,
		"destination_peer_id": t.DestinationPeerID,
		"bytes":               t.Bytes,
	}
	if t.Reason != "" {
		payload["reason"] = t.Reason
	}
	if len(t.Sources) > 0 {
		// Sorted, so the event is byte-identical for the same transfer however
		// the map happened to iterate — an event that differs run to run cannot
		// be compared, and comparing them is what a subscriber following one
		// blob across the fabric does.
		ids := make([]string, 0, len(t.Sources))
		for id := range t.Sources {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		contributions := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			contributions = append(contributions, map[string]any{
				"peer_id": id, "pieces": t.Sources[id],
			})
		}
		payload["sources"] = contributions
		payload["mode"] = "pieces"
	}
	return c.events.EmitTx(ctx, tx, events.TypeReplicationTransferChanged, "blob", t.BlobHash, payload)
}

// BlobSize is the size this node has recorded for a blob, in bytes.
//
// # Why a transfer needs it, and why it must come from HERE
//
// A piece transfer divides a blob by its SIZE, and both ends must divide it
// identically or they are naming different byte ranges under the same index
// (ADR-0043). The size therefore cannot come from whichever peer answered
// first: that would let a source choose the division, and a source that chose
// wrongly would be indistinguishable from one that was merely unlucky.
//
// It comes from the local catalogue, which learned it the same way it learned
// the blob exists at all — a peer's catalogue snapshot, reconciled. That makes
// it this node's own claim about the blob rather than a claim made during the
// transfer, which is the property that lets a disagreement be attributed.
//
// # When it is missing
//
// A blob nobody has ever described has no size here, and that is not a
// degenerate case: it is what asking for a blob this node has never heard of
// looks like. Refused with a named error rather than a zero, because a zero
// size divides into no pieces and would surface much later as an empty
// transfer that blamed the peers.
func (c *Catalog) BlobSize(ctx context.Context, blobHash string) (int64, error) {
	var size int64
	err := c.db.Reader().QueryRowContext(ctx,
		`SELECT size FROM blobs WHERE hash = ?`, blobHash).Scan(&size)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %s", ErrBlobSizeUnknown, blobHash)
	case err != nil:
		return 0, fmt.Errorf("catalog: reading the size of %s: %w", blobHash, err)
	}
	return size, nil
}

// ErrBlobSizeUnknown is a blob this node has no record of, asked about by
// something that needs its size.
var ErrBlobSizeUnknown = errors.New("catalog: this node has no recorded size for that blob")
