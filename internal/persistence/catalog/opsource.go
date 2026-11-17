package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// An op-sourced blob row (ADR-0119 amendment, #833; #800's prerequisite).
//
// replicate_blob pulls only a blob this node has a `blobs` row for, and only
// from a peer its `replicas` record as holding it (#658). On a media node both
// come from this node's own ingest or from a Full Peer's inventory, so a blob
// that only a sibling site holds can never be pulled: the sibling is a Partial
// Peer (ADR-0119), and no byte-moving loop tells this node what it holds.
//
// A signed catalog op can. A `hold` names the blob's digest and size and is
// signed by the peer that holds it, so it is that peer's claim about its own
// disk (ADR-0033) — the same claim an ADR-0114 placement offer makes, and it is
// recorded the same way: a `blobs` row adopted from the size hint, and a
// `present` replica for the signer. Both are metadata. The pull still hashes
// every byte against the digest it asked for (invariant 1), and the verified
// length replaces the hint (RecordBlobTransferred).

// OpSourcedBlob is a blob a verified catalog op names, as the op states it.
type OpSourcedBlob struct {
	// BlobHash is the op's digest, "blake3:<64 lowercase hex>" (ADR-0005).
	BlobHash string
	// Size is the op's size in bytes: a hint until the bytes verify.
	Size int64
	// SignerKey is the op's `by`, the signer's peer public key as
	// identity.FormatPublicKey renders it. Peer ids are node-local (ADR-0012),
	// so the signer is found by its key.
	SignerKey string
}

var (
	// ErrOpSignerNotAMember is an op-sourced blob whose signer is not a peer
	// of this node: there is nobody here to pull it from, and recording a
	// replica for a stranger would make one a source.
	ErrOpSignerNotAMember = errors.New("catalog: the op's signer is not a member of this node")
	// ErrOpSignerIsSelf is an op-sourced blob this node signed for itself. A
	// claim this node makes about its own disk is its own replica's business,
	// recorded by ingest or a transfer, never by an op it reads back.
	ErrOpSignerIsSelf = errors.New("catalog: the op was signed by this node")
)

// RecordOpSourcedBlob records the blob an op names and the signer's claim to
// hold it, in one transaction, and returns this node's id for the signer: the
// source a replicate_blob for the blob then pulls from.
//
// It adopts a `blobs` row from the size hint when there is none, and records a
// `present` replica for the signer, stamped as just reported. An existing row
// keeps its size: a row this node already had is at least as good as a hint.
// Re-recording the same claim changes only the report time and emits nothing.
//
// Emits blob.created for an adopted row and replica.present for a replica that
// moved to present (invariant 7).
func (c *Catalog) RecordOpSourcedBlob(ctx context.Context, b OpSourcedBlob) (string, error) {
	hash, err := hashing.Parse(b.BlobHash)
	if err != nil {
		return "", fmt.Errorf("catalog: op-sourced blob %q: %w", b.BlobHash, err)
	}
	if b.Size < 0 {
		return "", fmt.Errorf("catalog: op-sourced blob %s has a negative size %d", hash, b.Size)
	}
	pub, err := identity.ParsePublicKey(b.SignerKey)
	if err != nil {
		return "", fmt.Errorf("catalog: op-sourced blob %s: signer: %w", hash, err)
	}
	now := c.clock.Now().UTC().Format(timestampFormat)
	var (
		source  string
		pending []events.Event
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		source, pending = "", nil
		var self int
		switch err := tx.QueryRowContext(ctx,
			`SELECT id, is_self FROM peers WHERE public_key = ?`, []byte(pub)).Scan(&source, &self); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrOpSignerNotAMember, b.SignerKey)
		case err != nil:
			return fmt.Errorf("catalog: looking up the op's signer: %w", err)
		case self == 1:
			return ErrOpSignerIsSelf
		}

		adopted, err := adoptBlob(ctx, tx, hash.String(), b.Size, now)
		if err != nil {
			return err
		}
		if adopted {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", hash.String(), map[string]any{
				"size": b.Size, "op_sourced": true, "claimed_by": source,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, reported_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				reported_at = excluded.reported_at, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present'`,
			hash.String(), source, b.Size, now, now)
		if err != nil {
			return fmt.Errorf("catalog: recording %s's claimed replica of %s: %w", source, hash, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Already present: the claim is restated, which is the freshness
			// GC's last-copy rule reads (ADR-0018). Nothing moved.
			if _, err := tx.ExecContext(ctx,
				`UPDATE replicas SET reported_at = ? WHERE blob_hash = ? AND peer_id = ?`,
				now, hash.String(), source); err != nil {
				return fmt.Errorf("catalog: restating %s's replica of %s: %w", source, hash, err)
			}
			return nil
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaPresent, "blob", hash.String(), map[string]any{
			"peer_id": source, "bytes_present": b.Size, "op_sourced": true,
		})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return "", err
	}
	c.events.Publish(pending...)
	return source, nil
}
