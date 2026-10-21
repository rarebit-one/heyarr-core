package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
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
	// BlockGrabFailed is a release the download client could never START
	// fetching — the tracker refused the infohash, or the magnet resolved to
	// nothing. The bytes never arrived, so it is neither a verification nor an
	// ingest failure; it is a dead source. Blocking it stops the want
	// re-selecting the same unfetchable release every search (§64's grab-side
	// analogue of the verify loop 00018 breaks). Recorded distinctly because a
	// tracker rejection is arguably transient — a future policy may expire it,
	// where a hash mismatch is permanent.
	BlockGrabFailed BlockReason = "grab_failed"
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
	var (
		ev      events.Event
		created bool
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		ev, created, err = c.blockReleaseTx(ctx, tx, b)
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

// blockReleaseTx is BlockRelease inside a caller's transaction. The event is
// returned for the caller to publish once the transaction commits.
func (c *Catalog) blockReleaseTx(ctx context.Context, tx *sql.Tx, b BlockedRelease) (events.Event, bool, error) {
	if b.Reason == "" {
		b.Reason = BlockVerificationFailed
	}
	now := c.clock.Now().Format(timestampFormat)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO blocked_releases
			(id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (desired_item_id, provider, candidate_id) DO NOTHING`,
		uuid.Must(uuid.NewV7()).String(), b.DesiredItemID, b.Provider,
		b.CandidateID, b.Title, b.Detail, string(b.Reason), now)
	if err != nil {
		return events.Event{}, false, fmt.Errorf("catalog: blocking %s/%s: %w", b.Provider, b.CandidateID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return events.Event{}, false, err
	}
	if n == 0 {
		// Already blocked. Not a transition.
		return events.Event{}, false, nil
	}

	// Invariant 7, inside the transaction that wrote the row. A subscriber
	// watching acquisition.* needs this one: it is the difference between
	// "the search keeps choosing badly" and "the search is refusing to
	// choose the thing that keeps failing".
	ev, err := c.events.EmitTx(ctx, tx, events.TypeReleaseBlocked,
		"desired_item", b.DesiredItemID, map[string]any{
			"desired_item_id": b.DesiredItemID,
			"provider":        b.Provider,
			"candidate_id":    b.CandidateID,
			"title":           b.Title,
			"reason":          string(b.Reason),
			"detail":          b.Detail,
		})
	return ev, err == nil, err
}

// BlockTransferRelease blocks the release a want's in-flight transfer came
// from, and reports whether the transfer is still the want's (#819).
//
// One transaction, so a search running in parallel cannot change the answer
// halfway: the want must be in one of phases and its transfer row must still
// name provider/externalID. Only then is the selected candidate the release
// that produced this transfer, because a search would have moved the phase off
// idle (or replaced the row through its grab). The one change that moves the
// selection without the phase, a by-hand override in flight, is caught by its
// selected_at being later than the row's grab. ours=false means the transfer
// is no longer this want's to act on, and nothing was written. ours=true with
// no selected candidate, or a selection newer than the grab, blocks nothing:
// there is no release this transfer can be said to have come from.
func (c *Catalog) BlockTransferRelease(
	ctx context.Context, desiredItemID, provider, externalID string,
	phases []acquisition.Phase, detail string, reason BlockReason,
) (ours bool, err error) {
	var (
		ev      events.Event
		created bool
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		var phase string
		switch err := tx.QueryRowContext(ctx,
			`SELECT phase FROM acquisition_state WHERE desired_item_id = ?`, desiredItemID,
		).Scan(&phase); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if !slices.Contains(phases, acquisition.Phase(phase)) {
			return nil
		}
		var grabbedAt string
		switch err := tx.QueryRowContext(ctx, `
			SELECT created_at FROM acquisitions
			 WHERE desired_item_id = ? AND provider = ? AND external_id = ?`,
			desiredItemID, provider, externalID).Scan(&grabbedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		ours = true
		b := BlockedRelease{DesiredItemID: desiredItemID, Detail: detail, Reason: reason}
		var selectedAt sql.NullString
		switch err := tx.QueryRowContext(ctx, `
			SELECT provider, candidate_id, title, selected_at FROM release_candidates
			 WHERE desired_item_id = ? AND selected = 1`, desiredItemID,
		).Scan(&b.Provider, &b.CandidateID, &b.Title, &selectedAt); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		// A selection made AFTER this transfer was grabbed is not its release:
		// a by-hand override while it was in flight (which moves the selection
		// and not the phase), or a later search's pick. Blocking it would name
		// the wrong release, so nothing is blocked. A selection with no stamp
		// predates selected_at and has not changed since (every selection path
		// stamps it), so it is the transfer's. Compared as times: the two
		// columns are written in different layouts.
		if selectedAt.Valid {
			sel, serr := time.Parse(time.RFC3339Nano, selectedAt.String)
			grab, gerr := time.Parse(time.RFC3339Nano, grabbedAt)
			if serr == nil && gerr == nil && sel.After(grab) {
				return nil
			}
		}
		ev, created, err = c.blockReleaseTx(ctx, tx, b)
		return err
	})
	if err != nil {
		return false, err
	}
	if created {
		c.events.Publish(ev)
	}
	return ours, nil
}

// ErrNotBlocked is UnblockRelease asked to lift a block that is not there.
var ErrNotBlocked = errors.New("catalog: that release is not blocked for this want")

// ErrAmbiguousBlock is UnblockRelease given a candidate id that more than one
// provider has blocked for the want (or BlockByHand one that more than one
// provider offered), without saying which provider's.
var ErrAmbiguousBlock = errors.New("catalog: more than one provider blocked that candidate id for this want")

// UnblockRelease lifts a block: the want may choose candidateID again (#813).
//
// The operator's lever for a block that was wrong, most often one recorded
// before node-configuration faults stopped blocking releases (a missing
// library, a path map). Most candidate ids are unique only within their
// provider, so the block is named by provider and candidate. An empty provider
// means "the only provider that blocked this candidate", and ErrAmbiguousBlock
// when there is more than one. An infohash is the exception: it names the
// bytes whoever offered them, and the fan-out applier honours a block on it
// from any provider, so lifting one lifts it under every provider (a named
// provider must still be one of them). Emits acquisition.release_unblocked per
// lifted block, in the same transaction (invariant 7). ErrNotBlocked when there
// was nothing to lift.
func (c *Catalog) UnblockRelease(ctx context.Context, desiredItemID, provider, candidateID string) error {
	var emitted []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		providers, err := blockingProviders(ctx, tx, desiredItemID, candidateID)
		if err != nil {
			return err
		}
		if len(providers) == 0 || provider != "" && !slices.Contains(providers, provider) {
			return ErrNotBlocked
		}
		switch {
		case strings.HasPrefix(candidateID, "infohash:"):
		case provider != "":
			providers = []string{provider}
		case len(providers) > 1:
			return fmt.Errorf("%w: %s", ErrAmbiguousBlock, strings.Join(providers, ", "))
		}
		for _, p := range providers {
			b := BlockedRelease{DesiredItemID: desiredItemID, Provider: p, CandidateID: candidateID}
			var reason string
			if err := tx.QueryRowContext(ctx, `
				DELETE FROM blocked_releases
				 WHERE desired_item_id = ? AND provider = ? AND candidate_id = ?
				RETURNING title, reason`, desiredItemID, p, candidateID,
			).Scan(&b.Title, &reason); err != nil {
				return fmt.Errorf("catalog: unblocking %s: %w", candidateID, err)
			}
			ev, err := c.events.EmitTx(ctx, tx, events.TypeReleaseUnblocked,
				"desired_item", desiredItemID, map[string]any{
					"desired_item_id": desiredItemID,
					"provider":        b.Provider,
					"candidate_id":    b.CandidateID,
					"title":           b.Title,
					"was":             reason,
				})
			if err != nil {
				return err
			}
			emitted = append(emitted, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(emitted...)
	return nil
}

// ErrUnknownCandidate is BlockByHand asked to block a candidate id, with no
// provider named, that the want's last search did not offer.
var ErrUnknownCandidate = errors.New("catalog: that candidate id is not among this want's candidates")

// BlockByHand is the operator's block (#819): the want must not choose
// candidateID again, recorded as BlockManual. The other half of UnblockRelease.
//
// A candidate id is unique only within its provider, so the provider is
// resolved the same way an unblock resolves it: named, or the only provider
// whose candidate the want's last search offered under that id. Naming a
// provider also blocks a release the last search did not offer (a fan-out
// release, or one a later search may find). Idempotent: created is false when
// the release was already blocked.
func (c *Catalog) BlockByHand(
	ctx context.Context, desiredItemID, provider, candidateID, detail string,
) (b BlockedRelease, created bool, err error) {
	cands, err := c.CandidatesFor(ctx, desiredItemID)
	if err != nil {
		return BlockedRelease{}, false, err
	}
	var match []Candidate
	providers := map[string]bool{}
	for _, cd := range cands {
		if cd.CandidateID == candidateID && (provider == "" || cd.Provider == provider) {
			match = append(match, cd)
			providers[cd.Provider] = true
		}
	}
	switch {
	case provider == "" && len(match) == 0:
		return BlockedRelease{}, false, ErrUnknownCandidate
	case provider == "" && len(providers) > 1:
		names := make([]string, 0, len(providers))
		for p := range providers {
			names = append(names, p)
		}
		slices.Sort(names)
		return BlockedRelease{}, false, fmt.Errorf("%w: %s", ErrAmbiguousBlock, strings.Join(names, ", "))
	}
	b = BlockedRelease{
		DesiredItemID: desiredItemID, Provider: provider, CandidateID: candidateID,
		Detail: strings.TrimSpace(detail), Reason: BlockManual,
	}
	if len(match) > 0 {
		b.Provider, b.Title = match[0].Provider, match[0].Title
	}
	if b.Detail == "" {
		b.Detail = "blocked by hand"
	}
	created, err = c.BlockRelease(ctx, b)
	if err != nil || created {
		return b, created, err
	}
	// Already blocked: a repeat changes nothing, so report the block as it
	// stands rather than the one this call would have written.
	existing, err := c.BlockedKeys(ctx, desiredItemID)
	if err != nil {
		return BlockedRelease{}, false, err
	}
	if stored, ok := existing[b.Key()]; ok {
		return stored, false, nil
	}
	return b, false, nil
}

// blockingProviders lists the providers that blocked candidateID for a want.
func blockingProviders(ctx context.Context, tx *sql.Tx, desiredItemID, candidateID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT provider FROM blocked_releases
		 WHERE desired_item_id = ? AND candidate_id = ?
		 ORDER BY provider`, desiredItemID, candidateID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading blocks on %s: %w", candidateID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
