package catalog

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// The dead-swarm fallback (ADR-0119 #800 amendment) and #799: what a want that
// fan-out created came from, and what became of it here.

// IsSiblingKey reports whether key, a peer public key as an op's `by` carries
// it, is a current member of this node other than this node itself. A report
// from anyone else does not make the origin publish.
func (c *Catalog) IsSiblingKey(ctx context.Context, key string) (bool, error) {
	pub, err := identity.ParsePublicKey(key)
	if err != nil {
		return false, nil
	}
	var self int
	switch err := c.db.Reader().QueryRowContext(ctx,
		`SELECT is_self FROM peers WHERE public_key = ?`, []byte(pub)).Scan(&self); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: looking up a signer: %w", err)
	}
	return self == 0, nil
}

// FanOutStatus is the sibling's acquire op behind a want, and this node's
// outcomes for it.
type FanOutStatus struct {
	OpHash       string
	Site         string
	Infohash     string
	ReleaseTitle string
	Scope        string
	Aspect       string
	Outcome      FanOutOutcome
	Detail       string
	AppliedAt    time.Time
	// Reported is whether this node signed an unsourced op for the acquire:
	// told the origin it can resolve no source, so the origin holds it.
	Reported bool
	// Holds are this node's outcomes for the holds that name the want's work.
	Holds []HoldApplication
	// HoldLibraries names each hold's library, by op hash, "" when none.
	HoldLibraries map[string]string
}

// FanOutStatusFor reads the fan-out behind a want: the newest selection among
// the acquire ops it was applied for, with its outcome, whether this node
// reported it unsourced, and the hold outcomes for its work. found is false
// for a want fan-out did not create.
func (c *Catalog) FanOutStatusFor(ctx context.Context, desiredItemID string) (FanOutStatus, bool, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT o.op_hash, o.outcome, o.detail, o.applied_at, c.token
		  FROM catalog_op_applications o JOIN catalog_ops c ON c.op_hash = o.op_hash
		 WHERE o.desired_item_id = ? AND c.op = 'acquire'`, desiredItemID)
	if err != nil {
		return FanOutStatus{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
	}
	var (
		out   FanOutStatus
		found bool
		pick  catalogop.Op
	)
	for rows.Next() {
		var (
			s         FanOutStatus
			at, token string
		)
		if err := rows.Scan(&s.OpHash, &s.Outcome, &s.Detail, &at, &token); err != nil {
			_ = rows.Close()
			return FanOutStatus{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
		}
		op, err := catalogop.Verify(token)
		if err != nil || op.Acquire == nil {
			continue
		}
		// The newest selection, ties to the greater hash, as catalogop's view
		// picks the effective acquire.
		newer := !found || op.Acquire.SelectedAt.After(pick.Acquire.SelectedAt) ||
			(op.Acquire.SelectedAt.Equal(pick.Acquire.SelectedAt) && op.Hash > pick.Hash)
		if !newer {
			continue
		}
		s.AppliedAt = parseStamp(at)
		s.Site, s.Infohash, s.ReleaseTitle = op.Acquire.Site, op.Acquire.Infohash, op.Acquire.Title
		s.Scope, s.Aspect = op.Acquire.Scope, op.Acquire.Aspect
		out, pick, found = s, op, true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return FanOutStatus{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
	}
	_ = rows.Close()
	if !found {
		return FanOutStatus{}, false, nil
	}

	var selfKey []byte
	switch err := c.db.Reader().QueryRowContext(ctx,
		`SELECT public_key FROM peers WHERE is_self = 1`).Scan(&selfKey); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return FanOutStatus{}, false, fmt.Errorf("catalog: reading this node's key: %w", err)
	}
	if len(selfKey) == ed25519.PublicKeySize {
		var one int
		switch err := c.db.Reader().QueryRowContext(ctx, `
			SELECT 1 FROM catalog_ops c, json_each(c.prev) p
			 WHERE c.op = 'unsourced' AND c.signer = ? AND p.value = ? LIMIT 1`,
			identity.FormatPublicKey(ed25519.PublicKey(selfKey)), out.OpHash).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return FanOutStatus{}, false, fmt.Errorf("catalog: reading this node's reports: %w", err)
		default:
			out.Reported = true
		}
	}

	holds, err := c.db.Reader().QueryContext(ctx, `
		SELECT h.op_hash, h.blob_hash, COALESCE(h.library_id, ''), COALESCE(l.name, ''),
		       h.outcome, h.detail, h.applied_at
		  FROM catalog_hold_applications h
		  JOIN catalog_ops c ON c.op_hash = h.op_hash
		  LEFT JOIN libraries l ON l.id = h.library_id
		 WHERE c.content_type = ? AND c.work_key = ? AND h.outcome <> ?
		 ORDER BY h.applied_at, h.op_hash`,
		pick.Target.ContentType, pick.Target.WorkKey, string(HoldOwn))
	if err != nil {
		return FanOutStatus{}, false, fmt.Errorf("catalog: reading the holds for %s: %w", desiredItemID, err)
	}
	defer func() { _ = holds.Close() }()
	out.HoldLibraries = map[string]string{}
	for holds.Next() {
		var (
			h       HoldApplication
			lib, at string
		)
		if err := holds.Scan(&h.OpHash, &h.BlobHash, &h.LibraryID, &lib, &h.Outcome, &h.Detail, &at); err != nil {
			return FanOutStatus{}, false, fmt.Errorf("catalog: reading the holds for %s: %w", desiredItemID, err)
		}
		h.AppliedAt = parseStamp(at)
		out.Holds = append(out.Holds, h)
		out.HoldLibraries[h.OpHash] = lib
	}
	return out, true, holds.Err()
}
