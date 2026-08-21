package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Blocked releases (§64, M3-13).
//
// # The loop this breaks
//
// A verification failure means the release was bad. Without a mark that
// outlives the candidate set, the next search selects the same release, it
// fails to verify again, and the want downloads it forever — a bug that
// presents as bandwidth rather than as an error.
//
// The mark cannot live on the candidate row because RecordSearch replaces a
// want's candidates wholesale, and the next search is exactly when the mark
// needs to be read. See migration 00018.

// BlockReason is why a release was blocked.
type BlockReason string

const (
	// BlockVerificationFailed is bytes that did not hash to what was claimed.
	// The release was bad, and choosing it again would repeat the download.
	BlockVerificationFailed BlockReason = "verification_failed"
	// BlockIngestFailed is bytes that were fine and could not be brought under
	// management.
	//
	// Recorded distinctly from a verification failure because it is arguably a
	// LOCAL problem — a full disk, a permission — and blocking a good release
	// for it may well be wrong. Using one word for both would bake that
	// decision in now; two words leave it open to be made later on evidence.
	BlockIngestFailed BlockReason = "ingest_failed"
	// BlockManual is an operator's decision.
	BlockManual BlockReason = "manual"
)

// BlockedRelease is one release a want will not choose again.
type BlockedRelease struct {
	DesiredItemID string
	Provider      string
	CandidateID   string
	Title         string
	Detail        string
	Reason        BlockReason
}

// Key is the identity a search filters on. The provider is part of it because
// a candidate id is only unique within the indexer that issued it.
func (b BlockedRelease) Key() string { return b.Provider + "\x00" + b.CandidateID }

// BlockRelease records that a release must not be chosen again for this want.
//
// Idempotent: the job that discovers a failure will be re-run (invariant 9),
// and blocking twice is the same statement. A repeat is not an event — emitting
// per poll pass would turn the log into a heartbeat.
func (c *Catalog) BlockRelease(ctx context.Context, b BlockedRelease) (bool, error) {
	if b.Reason == "" {
		b.Reason = BlockVerificationFailed
	}
	now := c.clock.Now().Format(timestampFormat)

	var (
		ev      events.Event
		created bool
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO blocked_releases
				(id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (desired_item_id, provider, candidate_id) DO NOTHING`,
			uuid.Must(uuid.NewV7()).String(), b.DesiredItemID, b.Provider,
			b.CandidateID, b.Title, b.Detail, string(b.Reason), now)
		if err != nil {
			return fmt.Errorf("catalog: blocking %s/%s: %w", b.Provider, b.CandidateID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Already blocked. Not a transition.
			return nil
		}
		created = true

		// Invariant 7, inside the transaction that wrote the row. A subscriber
		// watching acquisition.* needs this one: it is the difference between
		// "the search keeps choosing badly" and "the search is refusing to
		// choose the thing that keeps failing".
		ev, err = c.events.EmitTx(ctx, tx, events.TypeReleaseBlocked,
			"desired_item", b.DesiredItemID, map[string]any{
				"desired_item_id": b.DesiredItemID,
				"provider":        b.Provider,
				"candidate_id":    b.CandidateID,
				"title":           b.Title,
				"reason":          string(b.Reason),
				"detail":          b.Detail,
			})
		return err
	})
	if err != nil {
		return false, err
	}
	if created {
		c.events.Publish(ev)
	}
	return created, nil
}

// BlockedFor lists what a want will not choose again.
//
// Returned as a slice rather than a set so the caller decides the shape it
// wants; the search job builds a lookup from it once per pass.
func (c *Catalog) BlockedFor(ctx context.Context, desiredItemID string) ([]BlockedRelease, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT desired_item_id, provider, candidate_id, title, detail, reason
		FROM blocked_releases WHERE desired_item_id = ?
		ORDER BY blocked_at, candidate_id`, desiredItemID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading blocked releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BlockedRelease
	for rows.Next() {
		var b BlockedRelease
		var reason string
		if err := rows.Scan(&b.DesiredItemID, &b.Provider, &b.CandidateID,
			&b.Title, &b.Detail, &reason); err != nil {
			return nil, err
		}
		b.Reason = BlockReason(reason)
		out = append(out, b)
	}
	return out, rows.Err()
}

// BlockedKeys is BlockedFor as a lookup, which is what a search actually wants.
func (c *Catalog) BlockedKeys(ctx context.Context, desiredItemID string) (map[string]BlockedRelease, error) {
	list, err := c.BlockedFor(ctx, desiredItemID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]BlockedRelease, len(list))
	for _, b := range list {
		out[b.Key()] = b
	}
	return out, nil
}

// ErrNoRootForContent means no enabled library root can hold this content type.
var ErrNoRootForContent = errors.New("catalog: no enabled library root for that content type")

// RootForContentType picks the library root a completed acquisition lands in.
//
// # Why an acquisition needs a root at all
//
// ingest.Pipeline is root-oriented, and rightly so: a root carries the content
// type identification parses against and the materialisation mode ADR-0014's
// ladder starts from. A completed download is not under a root — it is
// wherever the download client put it — so one has to be chosen.
//
// The honest choice is the library whose content type matches the Work being
// wanted. That is the same question the operator answered when they created
// the library, so it needs no new configuration.
//
// # The oldest enabled root, deterministically
//
// A library may have several roots and nothing yet says which should receive
// new acquisitions. Rather than invent a policy nothing configures, this takes
// the oldest enabled one and does so DETERMINISTICALLY — ordering by
// (created_at, id) so two nodes, or two runs, reach the same answer.
//
// A per-library "acquisition target" is the obvious eventual refinement, and
// deliberately not invented here: a setting nothing sets is a setting whose
// default is the only value anybody uses.
func (c *Catalog) RootForContentType(ctx context.Context, contentType string) (ingest.Root, error) {
	var rootID string
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT r.id
		FROM library_roots r JOIN libraries l ON l.id = r.library_id
		WHERE l.content_type = ? AND r.enabled = 1 AND l.enabled = 1
		  AND r.ingest_mode <> 'link'
		ORDER BY r.created_at, r.id
		LIMIT 1`, contentType).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		// Named precisely. "No such root" would send an operator looking for a
		// missing directory; the actual problem is that nothing is configured
		// to hold this kind of content, which is a library they have not made.
		return ingest.Root{}, fmt.Errorf(
			"%w: nothing is configured to hold %q — create a library for it",
			ErrNoRootForContent, contentType)
	}
	if err != nil {
		return ingest.Root{}, fmt.Errorf("catalog: choosing a root for %s: %w", contentType, err)
	}
	return c.Root(ctx, rootID)
}
