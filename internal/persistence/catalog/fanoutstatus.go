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
	out, pick, found, err := c.newestFanOut(ctx, desiredItemID)
	if err != nil || !found {
		return FanOutStatus{}, false, err
	}

	reported, err := c.reportedUnsourced(ctx)
	if err != nil {
		return FanOutStatus{}, false, err
	}
	out.Reported = reported[out.OpHash]

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

// newestFanOut is the newest selection among the acquire ops a want was
// applied for, with its recorded outcome.
func (c *Catalog) newestFanOut(ctx context.Context, desiredItemID string) (FanOutStatus, catalogop.Op, bool, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT o.op_hash, o.outcome, o.detail, o.applied_at, c.token
		  FROM catalog_op_applications o JOIN catalog_ops c ON c.op_hash = o.op_hash
		 WHERE o.desired_item_id = ? AND c.op = 'acquire'`, desiredItemID)
	if err != nil {
		return FanOutStatus{}, catalogop.Op{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out   FanOutStatus
		pick  catalogop.Op
		found bool
	)
	for rows.Next() {
		var (
			s         FanOutStatus
			at, token string
		)
		if err := rows.Scan(&s.OpHash, &s.Outcome, &s.Detail, &at, &token); err != nil {
			return FanOutStatus{}, catalogop.Op{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
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
		return FanOutStatus{}, catalogop.Op{}, false, fmt.Errorf("catalog: reading the fan-out of %s: %w", desiredItemID, err)
	}
	return out, pick, found, nil
}

// reportedUnsourced is the set of acquire op hashes this node told the origin
// it can resolve no source for: the ops its own `unsourced` ops cite.
func (c *Catalog) reportedUnsourced(ctx context.Context) (map[string]bool, error) {
	self, err := c.selfSigner(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if self == "" {
		return out, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT p.value FROM catalog_ops c, json_each(c.prev) p
		 WHERE c.op = 'unsourced' AND c.signer = ?`, self)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading this node's reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("catalog: reading this node's reports: %w", err)
		}
		out[hash] = true
	}
	return out, rows.Err()
}

// FanOutEntry is one sibling's acquire op this node evaluated and what it did
// with it, whether or not that reached a want (#799). scope_unresolved,
// tombstoned and most no_site_source outcomes never create one, so this is the
// only read that shows them.
type FanOutEntry struct {
	OpHash       string
	Site         string
	ContentType  string
	WorkKey      string
	WorkTitle    string
	Infohash     string
	ReleaseTitle string
	Scope        string
	Aspect       string
	SelectedAt   time.Time
	Outcome      FanOutOutcome
	Detail       string
	// DesiredItemID is the want the op was applied to, "" when none was.
	DesiredItemID string
	AppliedAt     time.Time
	// Reported is whether this node signed an unsourced op for it.
	Reported bool
}

// FanOutQuery filters ListFanOut. Zero fields match everything.
type FanOutQuery struct {
	Outcome FanOutOutcome
	Site    string
	// Signer narrows to the ops one key signed, as an op's `by` carries it: a
	// sibling asking over the peer surface sees only its own ops (#924).
	Signer string
	// OpHash narrows to one acquire op.
	OpHash string
	// Limit bounds the result; zero or less means no bound.
	Limit int
}

// ListFanOut lists the siblings' acquire ops this node has an outcome for,
// most recently applied first. This node's own ops (outcome own) are not
// fan-out here and are left out.
//
// The origin site lives in the signed op, not in a column, so it is filtered
// after verifying each token. The table holds one row per acquire op in the
// catalog op-log, which catalog sync already bounds.
func (c *Catalog) ListFanOut(ctx context.Context, q FanOutQuery) ([]FanOutEntry, error) {
	where, args := "c.op = 'acquire' AND o.outcome <> ?", []any{string(FanOutOwn)}
	if q.Outcome != "" {
		where += " AND o.outcome = ?"
		args = append(args, string(q.Outcome))
	}
	if q.Signer != "" {
		where += " AND c.signer = ?"
		args = append(args, q.Signer)
	}
	if q.OpHash != "" {
		where += " AND o.op_hash = ?"
		args = append(args, q.OpHash)
	}
	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT o.op_hash, COALESCE(o.desired_item_id, ''), o.outcome, o.detail, o.applied_at,
		       c.content_type, c.work_key, c.token
		  FROM catalog_op_applications o JOIN catalog_ops c ON c.op_hash = o.op_hash
		 WHERE `+where+`
		 ORDER BY o.applied_at DESC, o.op_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing fan-out: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FanOutEntry
	for rows.Next() {
		var (
			e         FanOutEntry
			at, token string
		)
		if err := rows.Scan(&e.OpHash, &e.DesiredItemID, &e.Outcome, &e.Detail, &at,
			&e.ContentType, &e.WorkKey, &token); err != nil {
			return nil, fmt.Errorf("catalog: listing fan-out: %w", err)
		}
		op, err := catalogop.Verify(token)
		if err != nil || op.Acquire == nil {
			continue
		}
		a := op.Acquire
		if q.Site != "" && a.Site != q.Site {
			continue
		}
		e.Site, e.WorkTitle, e.Infohash, e.ReleaseTitle = a.Site, a.WorkTitle, a.Infohash, a.Title
		e.Scope, e.Aspect, e.SelectedAt = a.Scope, a.Aspect, a.SelectedAt
		e.AppliedAt = parseStamp(at)
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing fan-out: %w", err)
	}
	_ = rows.Close()
	reported, err := c.reportedUnsourced(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Reported = reported[out[i].OpHash]
	}
	return out, nil
}

// selfSigner is this node's public key as an op's signer column carries it,
// "" when this node has no identity key.
func (c *Catalog) selfSigner(ctx context.Context) (string, error) {
	var selfKey []byte
	switch err := c.db.Reader().QueryRowContext(ctx,
		`SELECT public_key FROM peers WHERE is_self = 1`).Scan(&selfKey); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", fmt.Errorf("catalog: reading this node's key: %w", err)
	}
	if len(selfKey) != ed25519.PublicKeySize {
		return "", nil
	}
	return identity.FormatPublicKey(ed25519.PublicKey(selfKey)), nil
}

// Published is the origin's side of fan-out for a want (#799): the acquire op
// this node signed for it, and what came back from its siblings.
//
// What a sibling did with the op is recorded only at that sibling
// (catalog_op_applications is per node); the one answer that travels back on
// the op-log is an `unsourced` report, so that is all this read shows. The
// rest is asked of each sibling over the peer surface (ADR-0119 #924
// amendment).
type Published struct {
	OpHash       string
	Infohash     string
	ReleaseTitle string
	Scope        string
	Aspect       string
	Private      bool
	SelectedAt   time.Time
	// Unsourced are the current siblings that reported they can resolve no
	// source for this release, so this node holds it for them to pull.
	Unsourced []UnsourcedReport
	// Holds are the hold ops this node signed for the want's work.
	Holds []PublishedHold
}

// UnsourcedReport is one sibling's unsourced op for an acquire.
type UnsourcedReport struct {
	OpHash     string
	Site       string
	ReportedAt time.Time
}

// PublishedHold is one hold op this node signed: bytes it holds for a
// sibling to pull over the peer surface.
type PublishedHold struct {
	OpHash string
	Blob   string
	Scope  string
	HeldAt time.Time
}

// PublishedFor reads the newest acquire op this node signed for a want's
// (work, scope, aspect), the siblings that reported it unsourced, and the
// holds this node published for the work. found is false when this node
// published nothing for the want: it has no identity key, never grabbed a
// torrent for it, or the want came from a sibling.
func (c *Catalog) PublishedFor(ctx context.Context, desiredItemID string) (Published, bool, error) {
	self, err := c.selfSigner(ctx)
	if err != nil || self == "" {
		return Published{}, false, err
	}
	var (
		ct, wk, scope, aspect, language string
		editionKey, itemKey             sql.NullString
	)
	switch err := c.db.Reader().QueryRowContext(ctx, `
		SELECT w.content_type, w.work_key, d.scope, d.aspect, d.language, e.edition_key, i.item_key
		  FROM desired_items d
		  JOIN works w ON w.id = d.work_id
		  LEFT JOIN editions e ON e.id = d.edition_id
		  LEFT JOIN items i ON i.id = d.item_id
		 WHERE d.id = ?`, desiredItemID).
		Scan(&ct, &wk, &scope, &aspect, &language, &editionKey, &itemKey); {
	case errors.Is(err, sql.ErrNoRows):
		return Published{}, false, nil
	case err != nil:
		return Published{}, false, fmt.Errorf("catalog: reading want %s: %w", desiredItemID, err)
	}
	opScope, opAspect, err := opScopeAspect(desiredItemID, scope, aspect, language, editionKey, itemKey)
	if err != nil {
		return Published{}, false, err
	}

	ops, err := c.signedOps(ctx, `c.signer = ? AND c.op IN ('acquire', 'hold') AND c.content_type = ? AND c.work_key = ?`,
		self, ct, wk)
	if err != nil {
		return Published{}, false, err
	}
	var (
		out   Published
		pick  catalogop.Op
		found bool
	)
	for _, op := range ops {
		if op.Hold != nil {
			out.Holds = append(out.Holds, PublishedHold{
				OpHash: op.Hash, Blob: op.Hold.Blob, Scope: op.Hold.Scope, HeldAt: op.Hold.HeldAt,
			})
			continue
		}
		a := op.Acquire
		if a == nil || a.Scope != opScope || a.Aspect != opAspect {
			continue
		}
		// The newest selection, ties to the greater hash, as catalogop's view
		// picks the effective acquire.
		if found && (a.SelectedAt.Before(pick.Acquire.SelectedAt) ||
			(a.SelectedAt.Equal(pick.Acquire.SelectedAt) && op.Hash < pick.Hash)) {
			continue
		}
		pick, found = op, true
	}
	if !found {
		return Published{}, false, nil
	}
	a := pick.Acquire
	out.OpHash, out.Infohash, out.ReleaseTitle = pick.Hash, a.Infohash, a.Title
	out.Scope, out.Aspect, out.Private, out.SelectedAt = a.Scope, a.Aspect, a.Private, a.SelectedAt

	reports, err := c.signedOps(ctx, `c.op = 'unsourced' AND EXISTS
		(SELECT 1 FROM json_each(c.prev) p WHERE p.value = ?)`, pick.Hash)
	if err != nil {
		return Published{}, false, err
	}
	for _, r := range reports {
		if r.Unsourced == nil || r.Unsourced.Acquire != pick.Hash {
			continue
		}
		// A report from anyone but a current sibling makes the origin hold
		// nothing (IsSiblingKey), so it is not shown as one.
		sibling, err := c.IsSiblingKey(ctx, r.By)
		if err != nil {
			return Published{}, false, err
		}
		if sibling {
			out.Unsourced = append(out.Unsourced, UnsourcedReport{OpHash: r.Hash, Site: r.Unsourced.Site, ReportedAt: r.IssuedAt})
		}
	}
	return out, true, nil
}

// signedOps reads and verifies the op-log rows matching where, in issue
// order. A token that does not verify is skipped, as catalogop's view skips it.
func (c *Catalog) signedOps(ctx context.Context, where string, args ...any) ([]catalogop.Op, error) {
	//nolint:gosec // where is a literal fragment from this file; every value is bound
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT c.token FROM catalog_ops c WHERE `+where+` ORDER BY c.iat, c.op_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the op-log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []catalogop.Op
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, fmt.Errorf("catalog: reading the op-log: %w", err)
		}
		op, err := catalogop.Verify(token)
		if err != nil {
			continue
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
