package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Acquisitions — the bridge between a want and an external transfer (§58,
// M3-10).
//
// # Idempotency is enforced here, by the database
//
// The poll job WILL be re-run (invariant 9), and re-running it must not queue a
// second copy of a transfer already downloading. That is guaranteed by the
// UNIQUE on desired_item_id and by an upsert keyed on (provider, external_id) —
// not by the job remembering to check first, which is a guarantee that holds
// until two workers run at once.

// Acquisition is one want's in-flight transfer.
type Acquisition struct {
	ID            string
	DesiredItemID string
	Provider      string
	// ExternalID is the download client's own identifier — an infohash, never
	// a name and never a per-session numeric id.
	ExternalID   string
	ExternalName string
	RemotePath   string
	LocalPath    string
	BytesTotal   int64
	BytesDone    int64
	Trouble      string
}

// ErrNoAcquisitionRow is returned when a want has no in-flight transfer.
var ErrNoAcquisitionRow = errors.New("catalog: no acquisition for that desired item")

const acquisitionCols = `id, desired_item_id, provider, external_id, external_name,
	remote_path, local_path, bytes_total, bytes_done, trouble`

func scanAcquisitionRow(row interface{ Scan(...any) error }) (Acquisition, error) {
	var a Acquisition
	if err := row.Scan(&a.ID, &a.DesiredItemID, &a.Provider, &a.ExternalID,
		&a.ExternalName, &a.RemotePath, &a.LocalPath,
		&a.BytesTotal, &a.BytesDone, &a.Trouble); err != nil {
		return Acquisition{}, err
	}
	return a, nil
}

// RecordAcquisition creates or updates the link between a want and a transfer.
//
// Upsert on (provider, external_id) rather than insert-then-catch: the poll job
// runs repeatedly over the same transfers by design, so "already there" is the
// normal case and treating it as a conflict would make the common path an
// error path.
//
// Returns whether a row was CREATED, so the caller can emit only on the
// transition. A poll pass over an unchanged queue must emit nothing, or the
// event log becomes a heartbeat.
func (c *Catalog) RecordAcquisition(ctx context.Context, a Acquisition) (bool, error) {
	now := sqlite.FormatTimestamp(c.clock.Now())
	var created bool

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		// A want holds ONE acquisition (acquisitions.desired_item_id is UNIQUE).
		// When a want re-grabs a DIFFERENT release — its selected candidate
		// changed, so the transfer it points at changes — the row for the old
		// transfer must give way, or the insert below collides on
		// desired_item_id and the grab wedges forever retrying (the transfer is
		// already handed to the client, but its row can never be written). The
		// upsert's ON CONFLICT is keyed on (provider, external_id), so it does
		// NOT catch this: the new transfer has a new external_id, so there is no
		// conflict to update — it is a fresh insert, and it is the UNIQUE on
		// desired_item_id that rejects it. Clear the want's prior row first.
		//
		// A poll re-observing the SAME transfer keeps its row: provider AND
		// external_id match, so the NOT (…) is false and this deletes nothing;
		// the ON CONFLICT update below then carries it as before.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM acquisitions
			 WHERE desired_item_id = ? AND NOT (provider = ? AND external_id = ?)`,
			a.DesiredItemID, a.Provider, a.ExternalID); err != nil {
			return err
		}

		// Existence is checked INSIDE the transaction rather than inferred
		// afterwards.
		//
		// The tempting shortcut is to compare created_at to this pass's
		// timestamp — a row stamped now was made now. It is wrong under an
		// injected clock (ADR-0017), where every pass shares one timestamp and
		// so every pass looks like a creation. That is not a test artefact: it
		// is the same bug a coarse clock would produce in production, and the
		// fixed clock is what made it visible.
		//
		// SQLite's RowsAffected cannot help either — it reports 1 for an
		// insert and 1 for an ON CONFLICT update alike.
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM acquisitions WHERE provider = ? AND external_id = ?`,
			a.Provider, a.ExternalID).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
		case err != nil:
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO acquisitions
				(id, desired_item_id, provider, external_id, external_name,
				 remote_path, local_path, bytes_total, bytes_done, trouble,
				 created_at, updated_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider, external_id) DO UPDATE SET
				external_name = excluded.external_name,
				remote_path   = excluded.remote_path,
				local_path    = excluded.local_path,
				bytes_total   = excluded.bytes_total,
				bytes_done    = excluded.bytes_done,
				trouble       = excluded.trouble,
				updated_at    = excluded.updated_at,
				last_seen_at  = excluded.last_seen_at`,
			a.ID, a.DesiredItemID, a.Provider, a.ExternalID, a.ExternalName,
			a.RemotePath, a.LocalPath, a.BytesTotal, a.BytesDone, a.Trouble,
			now, now, now)
		if err != nil {
			return fmt.Errorf("catalog: recording acquisition %s: %w", a.ExternalID, err)
		}
		return nil
	})
	return created, err
}

// AcquisitionFor reads a want's in-flight transfer.
func (c *Catalog) AcquisitionFor(ctx context.Context, desiredItemID string) (Acquisition, error) {
	a, err := scanAcquisitionRow(c.db.Reader().QueryRowContext(ctx,
		`SELECT `+acquisitionCols+` FROM acquisitions WHERE desired_item_id = ?`,
		desiredItemID))
	if errors.Is(err, sql.ErrNoRows) {
		return Acquisition{}, ErrNoAcquisitionRow
	}
	return a, err
}

// AcquisitionByExternal reads one by the download client's identifier.
func (c *Catalog) AcquisitionByExternal(
	ctx context.Context, provider, externalID string,
) (Acquisition, error) {
	a, err := scanAcquisitionRow(c.db.Reader().QueryRowContext(ctx,
		`SELECT `+acquisitionCols+` FROM acquisitions
		 WHERE provider = ? AND external_id = ?`, provider, externalID))
	if errors.Is(err, sql.ErrNoRows) {
		return Acquisition{}, ErrNoAcquisitionRow
	}
	return a, err
}

// Acquisitions lists everything in flight, oldest first.
func (c *Catalog) Acquisitions(ctx context.Context) ([]Acquisition, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT `+acquisitionCols+` FROM acquisitions ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing acquisitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Acquisition
	for rows.Next() {
		a, err := scanAcquisitionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OrphanedDownload is a want stuck in an in-flight download phase whose transfer
// the download client no longer reports — a torrent removed out from under
// Heyarr (in the client's UI, by another tool, by a data-loss restart). Its
// acquisition row has not been refreshed by a poll for at least the grace
// window, so the transfer was not present the last time its client answered.
type OrphanedDownload struct {
	DesiredItemID string
	Provider      string
	ExternalID    string
	ExternalName  string
	Phase         acquisition.Phase
}

// OrphanedDownloads lists wants in QUEUED or DOWNLOADING whose acquisition row
// has not been seen (last_seen_at refreshed) for at least `grace`.
//
// Every present transfer refreshes last_seen_at on the poll pass that observes
// it (RecordAcquisition), so at the end of a pass a still-running download has
// last_seen_at == now and can never appear here. A row that IS stale was absent
// from its client's queue — either the torrent is gone, or that client did not
// answer this pass. The caller distinguishes those: it only fails orphans whose
// client actually responded, so an unreachable client does not strand its wants.
// The grace absorbs a single transient read that omits a transfer that still
// exists; it should exceed a couple of poll intervals.
func (c *Catalog) OrphanedDownloads(ctx context.Context, grace time.Duration) ([]OrphanedDownload, error) {
	cutoff := sqlite.FormatTimestamp(c.clock.Now().Add(-grace))
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT a.desired_item_id, a.provider, a.external_id, a.external_name, s.phase
		FROM acquisitions a
		JOIN acquisition_state s ON s.desired_item_id = a.desired_item_id
		WHERE s.phase IN (?, ?) AND a.last_seen_at < ?
		ORDER BY a.created_at, a.id`,
		string(acquisition.PhaseQueued), string(acquisition.PhaseDownloading), cutoff)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing orphaned downloads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []OrphanedDownload
	for rows.Next() {
		var o OrphanedDownload
		var phase string
		if err := rows.Scan(&o.DesiredItemID, &o.Provider, &o.ExternalID,
			&o.ExternalName, &phase); err != nil {
			return nil, err
		}
		o.Phase = acquisition.Phase(phase)
		out = append(out, o)
	}
	return out, rows.Err()
}

// StuckIngest is a want parked in VERIFYING or INGESTING whose phase has not
// changed for a while — a download that finished but whose hash-and-import
// never completed and is not being retried.
type StuckIngest struct {
	DesiredItemID string
	Phase         acquisition.Phase
}

// StuckIngests lists wants in VERIFYING or INGESTING whose phase_entered_at is
// older than `grace` — a wedged ingest.
//
// polldownloads enqueues the ingest exactly once, on the transition into
// VERIFYING, because queueing it every pass would fill the queue with work that
// is already done. That is correct while the job survives — but if it does not
// (a worker crash mid-lease, a node OOM, a download client that dropped the
// completed transfer before the ingest ran), nothing re-enqueues it and the
// want sits in INGESTING forever with no job in any state driving it. This is
// the read behind the watchdog that re-drives them.
//
// phase_entered_at (indexed with phase, migration 00014) dates the stall, and
// the grace absorbs a legitimately slow verify of a large file — a genuine
// verify/ingest holds its dedupe key live throughout, so re-enqueueing a want
// found here is idempotent regardless.
func (c *Catalog) StuckIngests(ctx context.Context, grace time.Duration) ([]StuckIngest, error) {
	cutoff := sqlite.FormatTimestamp(c.clock.Now().Add(-grace))
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT desired_item_id, phase
		FROM acquisition_state
		WHERE phase IN (?, ?) AND phase_entered_at < ?
		ORDER BY phase_entered_at, desired_item_id`,
		string(acquisition.PhaseVerifying), string(acquisition.PhaseIngesting), cutoff)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing stuck ingests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StuckIngest
	for rows.Next() {
		var s StuckIngest
		var phase string
		if err := rows.Scan(&s.DesiredItemID, &phase); err != nil {
			return nil, err
		}
		s.Phase = acquisition.Phase(phase)
		out = append(out, s)
	}
	return out, rows.Err()
}

// StuckGrab is a want stranded at idle after its grab failed. A poll failed the
// selected release back to idle — a tracker rejection ("info hash is not
// authorized"), a fetch that returned nothing — and left a troubled acquisition
// row behind. Nothing re-drives it on its own: reconcile only sets satisfaction,
// sweepOrphanedDownloads only looks at in-flight phases, and the search beat is
// gated by a backoff that can be a day out AND would re-select the very release
// that just failed. It carries the SELECTED candidate so the caller can block
// that release before letting a fresh search pick a different one.
type StuckGrab struct {
	DesiredItemID string
	// The selected candidate — the release whose grab failed. Provider and
	// CandidateID are the INDEXER's, keyed the way blocked_releases and the
	// search filter are — NOT the download client's provider/infohash carried on
	// the acquisitions row.
	Provider    string
	CandidateID string
	Title       string
	// Trouble is what the transfer last reported, recorded as the block's detail.
	Trouble string
}

// StuckGrabs lists wants stranded at idle after a failed grab: phase idle, not
// satisfied, a selected candidate, and a leftover acquisition row that carries a
// trouble string, entered idle at least `grace` ago.
//
// The grace is measured on phase_entered_at — when the fail landed the want at
// idle — so a want that only just fell back, and might still be moved by the
// same poll pass, is left for the next one. A want with NO selected candidate is
// deliberately absent: without the release in hand to block, re-driving would
// re-pick and re-fail, so those stay the search beat's business.
func (c *Catalog) StuckGrabs(ctx context.Context, grace time.Duration) ([]StuckGrab, error) {
	cutoff := sqlite.FormatTimestamp(c.clock.Now().Add(-grace))
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT s.desired_item_id, rc.provider, rc.candidate_id, rc.title, a.trouble
		FROM acquisition_state s
		JOIN acquisitions a ON a.desired_item_id = s.desired_item_id
		JOIN release_candidates rc
		  ON rc.desired_item_id = s.desired_item_id AND rc.selected = 1
		WHERE s.phase = ?
		  AND s.content <> 'satisfied'
		  AND a.trouble <> ''
		  AND s.phase_entered_at < ?
		ORDER BY s.phase_entered_at, s.desired_item_id`,
		string(acquisition.PhaseIdle), cutoff)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing stuck grabs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StuckGrab
	for rows.Next() {
		var g StuckGrab
		if err := rows.Scan(&g.DesiredItemID, &g.Provider, &g.CandidateID,
			&g.Title, &g.Trouble); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DropAcquisition removes the link between a want and a transfer.
//
// It does NOT touch the download client. Removing a transfer is a separate
// decision from forgetting about it — see downloads.Client.Remove's deleteData
// — and conflating them here would mean forgetting an acquisition could delete
// bytes Heyarr has not ingested yet.
func (c *Catalog) DropAcquisition(ctx context.Context, desiredItemID string) error {
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM acquisitions WHERE desired_item_id = ?`, desiredItemID)
		return err
	})
}

// TransferToAcquisition maps a provider's value type onto a row.
//
// The mapping lives here rather than in internal/downloads so that the download
// client stays free of persistence, and in the catalog rather than the worker
// so two callers cannot map it differently.
func TransferToAcquisition(
	id, desiredItemID, provider string, t providers.Transfer, localPath string,
) Acquisition {
	return Acquisition{
		ID:            id,
		DesiredItemID: desiredItemID,
		Provider:      provider,
		ExternalID:    t.ID,
		ExternalName:  t.Name,
		RemotePath:    t.Path,
		LocalPath:     localPath,
		BytesTotal:    t.BytesTotal,
		BytesDone:     t.BytesDone,
		Trouble:       t.Error,
	}
}
