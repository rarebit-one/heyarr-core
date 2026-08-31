package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Followed sources — the storage half (§55, M12, ADR-0057).
//
// The domain (internal/domain/followed) states what a subscription IS and how
// it projects wants; this is where a subscription lives and how the follow beat
// finds which are due a poll. The split is the same one every beat in this
// package takes: the policy — the cadence, the backoff, the deterministic
// spread — is pure and reused from acquisition.Schedule (FeedPoll), and only the
// query that finds what is due and the writes that record a poll live here.

// StoredSource is a followed source as persisted, its domain value plus the
// poll bookkeeping and timestamps that are storage's business.
type StoredSource struct {
	followed.Source
	LastPolledAt time.Time
	NextPollAt   time.Time
	Fruitless    int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateFollowSource inserts a validated subscription and emits its creation
// event (invariant 7). It leaves next_poll_at NULL, which is the most-due state
// there is — a source nobody has polled is polled first — so the follow beat
// picks a fresh subscription up on its next tick.
//
// The insert is plain: a second subscription over the same (work, feed) returns
// the unique-violation error, so following the same series through the same feed
// twice is refused rather than silently duplicated (§61's shape, one level up).
func (c *Catalog) CreateFollowSource(ctx context.Context, src followed.Source) (StoredSource, error) {
	if err := src.Validate(); err != nil {
		return StoredSource{}, err
	}
	if src.ID == "" {
		src.ID = uuid.Must(uuid.NewV7()).String()
	}
	now := c.clock.Now().UTC()
	stamp := now.Format(timestampFormat)
	monitor := 0
	if src.Monitor {
		monitor = 1
	}

	var ev events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO follow_sources
				(id, work_id, type, feed_ref, quality_profile_id, monitor, backfill,
				 reason, poll_schedule, poll_fruitless, last_polled_at, next_poll_at,
				 created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, NULL, ?, ?)`,
			src.ID, src.WorkID, string(src.Type), src.FeedRef, src.QualityProfileID,
			monitor, string(src.Backfill), src.Reason, followed.FeedPoll().Name, stamp, stamp); err != nil {
			return fmt.Errorf("catalog: inserting a followed source: %w", err)
		}
		var err error
		ev, err = c.events.EmitTx(ctx, tx, events.TypeFollowSourceCreated,
			"follow_source", src.ID, map[string]any{
				"follow_source_id":   src.ID,
				"work_id":            src.WorkID,
				"type":               string(src.Type),
				"quality_profile_id": src.QualityProfileID,
				"monitor":            src.Monitor,
				"backfill":           string(src.Backfill),
			})
		return err
	})
	if err != nil {
		return StoredSource{}, err
	}
	c.events.Publish(ev)

	return StoredSource{Source: src, CreatedAt: now, UpdatedAt: now}, nil
}

const followSourceColumns = `id, work_id, type, feed_ref, quality_profile_id, monitor,
	backfill, reason, poll_fruitless, coalesce(last_polled_at, ''),
	coalesce(next_poll_at, ''), created_at, updated_at`

func scanFollowSource(row interface{ Scan(...any) error }) (StoredSource, error) {
	var (
		s                        StoredSource
		monitor                  int
		last, next, created, upd string
	)
	if err := row.Scan(&s.ID, &s.WorkID, &s.Type, &s.FeedRef, &s.QualityProfileID,
		&monitor, &s.Backfill, &s.Reason, &s.Fruitless, &last, &next, &created, &upd); err != nil {
		return StoredSource{}, err
	}
	s.Monitor = monitor == 1
	if last != "" {
		s.LastPolledAt, _ = time.Parse(timestampFormat, last)
	}
	if next != "" {
		s.NextPollAt, _ = time.Parse(timestampFormat, next)
	}
	s.CreatedAt, _ = time.Parse(timestampFormat, created)
	s.UpdatedAt, _ = time.Parse(timestampFormat, upd)
	return s, nil
}

// ErrNoFollowSource is returned when a subscription id names nothing.
var ErrNoFollowSource = errors.New("catalog: no followed source with that id")

// FollowSource reads one subscription.
func (c *Catalog) FollowSource(ctx context.Context, id string) (StoredSource, error) {
	s, err := scanFollowSource(c.db.Reader().QueryRowContext(ctx,
		`SELECT `+followSourceColumns+` FROM follow_sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return StoredSource{}, ErrNoFollowSource
	}
	if err != nil {
		return StoredSource{}, fmt.Errorf("catalog: reading a followed source: %w", err)
	}
	return s, nil
}

// ListFollowSources lists every subscription, oldest first.
func (c *Catalog) ListFollowSources(ctx context.Context) ([]StoredSource, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT `+followSourceColumns+` FROM follow_sources ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing followed sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StoredSource
	for rows.Next() {
		s, err := scanFollowSource(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: reading a followed source: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FollowStats counts what a source has produced, for the followed listing:
// how many Items its feed has yielded, and how many of those are archived —
// i.e. have an item-scoped want whose content is satisfied. Counted over the
// source's Work, because that is what an Item and a projected want both anchor
// to (§11, ADR-0056).
func (c *Catalog) FollowStats(ctx context.Context, workID string) (known, archived int, err error) {
	if err := c.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM items WHERE work_id = ?`, workID).Scan(&known); err != nil {
		return 0, 0, fmt.Errorf("catalog: counting items for a work: %w", err)
	}
	if err := c.db.Reader().QueryRowContext(ctx, `
		SELECT count(*)
		FROM desired_items d
		JOIN acquisition_state a ON a.desired_item_id = d.id
		WHERE d.scope = 'item' AND d.work_id = ? AND a.content = 'satisfied'`,
		workID).Scan(&archived); err != nil {
		return 0, 0, fmt.Errorf("catalog: counting archived items for a work: %w", err)
	}
	return known, archived, nil
}

// DeleteFollowSource removes a subscription and emits its removal (invariant 7).
// It does NOT touch the Items it discovered or the wants it projected: unfollowing
// stops future polls, and whether to keep what was already archived is the
// caller's policy (keep_archive), not this method's. Reports whether a row existed.
func (c *Catalog) DeleteFollowSource(ctx context.Context, id string) (bool, error) {
	var (
		existed bool
		ev      events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var workID string
		err := tx.QueryRowContext(ctx,
			`SELECT work_id FROM follow_sources WHERE id = ?`, id).Scan(&workID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM follow_sources WHERE id = ?`, id); err != nil {
			return fmt.Errorf("catalog: deleting a followed source: %w", err)
		}
		existed = true
		ev, err = c.events.EmitTx(ctx, tx, events.TypeFollowSourceRemoved,
			"follow_source", id, map[string]any{"follow_source_id": id, "work_id": workID})
		return err
	})
	if err != nil {
		return false, err
	}
	if existed {
		c.events.Publish(ev)
	}
	return existed, nil
}

// DueSource is one subscription the follow beat should poll now.
type DueSource struct {
	SourceID string
	// Schedule is the feed-poll cadence, carried so the caller records the
	// attempt against the same schedule it decided with — the same shape a
	// DueSearch takes.
	Schedule acquisition.Schedule
	// Fruitless is the stored count of consecutive polls that discovered no new
	// item — the exponent for the backoff. Unlike a search's, it is stored rather
	// than derived, because "did the last poll find anything new" has no resting
	// state to read it back from: the poll worker records it (RecordPollOutcome).
	Fruitless int
	// FirstEver is true when the source has never been polled (next_poll_at NULL).
	FirstEver bool
}

// DueSources lists the subscriptions due a poll as of now, most overdue first so
// a limit truncates the least urgent. A source with next_poll_at NULL has never
// been polled and is the most due there is.
func (c *Catalog) DueSources(ctx context.Context, now time.Time, limit int) ([]DueSource, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT id, poll_fruitless, coalesce(next_poll_at, '')
		FROM follow_sources
		WHERE next_poll_at IS NULL OR next_poll_at <= ?
		ORDER BY coalesce(next_poll_at, ''), id
		LIMIT ?`, sortable(now), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing sources due a poll: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueSource
	for rows.Next() {
		var (
			id, storedNext string
			fruitless      int
		)
		if err := rows.Scan(&id, &fruitless, &storedNext); err != nil {
			return nil, fmt.Errorf("catalog: reading a source due a poll: %w", err)
		}
		out = append(out, DueSource{
			SourceID:  id,
			Schedule:  followed.FeedPoll(),
			Fruitless: fruitless,
			FirstEver: storedNext == "",
		})
	}
	return out, rows.Err()
}

// RecordPollScheduled advances a source's next_poll_at when the follow beat
// enqueues a poll for it — the provisional advance that stops the same source
// being re-enqueued on every tick while its poll is still queued or running.
//
// It is a compare-and-set on the row still being due, exactly as
// RecordSearchScheduled is: two beats running the same pass advance the row once
// between them, and the loser gets false. The AUTHORITATIVE next_poll_at — the
// one that reflects whether the poll actually found anything — is written by the
// worker (RecordPollOutcome); this only keeps the queue from filling with
// duplicate polls in the window before that runs.
func (c *Catalog) RecordPollScheduled(
	ctx context.Context, sourceID string, s acquisition.Schedule,
	fruitless int, now, next time.Time,
) (bool, error) {
	if sourceID == "" {
		return false, errors.New("catalog: recording a scheduled poll needs a source")
	}
	res, err := c.db.Writer().ExecContext(ctx, `
		UPDATE follow_sources
		   SET poll_schedule = ?, next_poll_at = ?, updated_at = ?
		 WHERE id = ?
		   AND (next_poll_at IS NULL OR next_poll_at <= ?)`,
		s.Name, sortable(next), sortable(now), sourceID, sortable(now))
	if err != nil {
		return false, fmt.Errorf("catalog: recording a scheduled poll for %s: %w", sourceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("catalog: recording a scheduled poll for %s: %w", sourceID, err)
	}
	return n > 0, nil
}

// RecordPollOutcome writes the authoritative schedule after a poll ran: the
// fruitless streak resets to zero when the poll discovered a new item and grows
// by one otherwise, and next_poll_at is set from that streak's backoff. It is
// idempotent in effect — re-running a poll recomputes the same outcome — which is
// what invariant 9 requires of the job that calls it.
func (c *Catalog) RecordPollOutcome(
	ctx context.Context, sourceID string, foundNew bool, now time.Time,
) error {
	if sourceID == "" {
		return errors.New("catalog: recording a poll outcome needs a source")
	}
	var fruitless int
	err := c.db.Reader().QueryRowContext(ctx,
		`SELECT poll_fruitless FROM follow_sources WHERE id = ?`, sourceID).Scan(&fruitless)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoFollowSource
	}
	if err != nil {
		return fmt.Errorf("catalog: reading a source's poll streak: %w", err)
	}
	if foundNew {
		fruitless = 0
	} else {
		fruitless++
	}
	next := followed.FeedPoll().NextAt(now, fruitless, sourceID)
	if _, err := c.db.Writer().ExecContext(ctx, `
		UPDATE follow_sources
		   SET poll_fruitless = ?, last_polled_at = ?, next_poll_at = ?, updated_at = ?
		 WHERE id = ?`,
		fruitless, sortable(now), sortable(next), sortable(now), sourceID); err != nil {
		return fmt.Errorf("catalog: recording a poll outcome for %s: %w", sourceID, err)
	}
	return nil
}

// MetadataHealth reports what the last health pass observed about the providers
// that can enumerate a feed (CapabilityMetadata), keyed by provider name. It is
// the follow beat's hold-off signal, the sibling of IndexerHealth: a beat that
// polls while every metadata provider is unhealthy fills the queue with work
// that will fail, so it holds off — and, like the search beat, a provider that
// has never been checked has no row and is UNKNOWN, not unhealthy, so a fresh
// install is not silent while it waits for a health pass nothing has run.
func (c *Catalog) MetadataHealth(ctx context.Context) (map[string]providers.Health, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT name, capabilities, healthy, detail, version, checked_at
		FROM provider_health`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading metadata provider health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]providers.Health{}
	for rows.Next() {
		var (
			name, caps, detail, version string
			healthy                     int
			checkedAt                   sql.NullString
		)
		if err := rows.Scan(&name, &caps, &healthy, &detail, &version, &checkedAt); err != nil {
			return nil, fmt.Errorf("catalog: reading metadata provider health: %w", err)
		}
		var declared []string
		if err := json.Unmarshal([]byte(caps), &declared); err != nil {
			c.log.Warn("a provider health row has undecodable capabilities",
				"provider", name, "error", err)
			continue
		}
		if !contains(declared, string(providers.CapabilityMetadata)) {
			continue
		}
		h := providers.Health{Healthy: healthy == 1, Detail: detail, Version: version}
		if checkedAt.Valid {
			if t, err := time.Parse(timestampFormat, checkedAt.String); err == nil {
				h.CheckedAt = t
			}
		}
		out[name] = h
	}
	return out, rows.Err()
}
