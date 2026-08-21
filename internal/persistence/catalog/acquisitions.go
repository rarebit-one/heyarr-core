package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	now := c.clock.Now().Format(timestampFormat)
	var created bool

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
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
