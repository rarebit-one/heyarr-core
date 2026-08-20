package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

var _ scanner.Store = (*Catalog)(nil)

// Fingerprints returns every cached fingerprint for a root.
//
// One query rather than a lookup per file: a scan of a large library would
// otherwise be a million round trips to answer a question the database can
// answer once. The cost is holding the root's cache in memory for the length of
// the scan, which for a million files is tens of megabytes — the trade that
// makes a rescan take seconds.
func (c *Catalog) Fingerprints(ctx context.Context, rootID string) (map[string]scanner.Fingerprint, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT path, size, mtime_ns, dev, inode FROM scanned_files WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the fingerprint cache for root %s: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]scanner.Fingerprint{}
	for rows.Next() {
		var fp scanner.Fingerprint
		if err := rows.Scan(&fp.RelPath, &fp.Size, &fp.MtimeNS, &fp.Dev, &fp.Inode); err != nil {
			return nil, fmt.Errorf("catalog: reading the fingerprint cache for root %s: %w", rootID, err)
		}
		out[fp.RelPath] = fp
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading the fingerprint cache for root %s: %w", rootID, err)
	}
	return out, nil
}

// BeginScan opens a scan_runs row.
//
// It first closes any run left behind by a process that died mid-scan. The
// one-live-run-per-root index is what stops two scans interleaving their
// fingerprint writes, and without this a single SIGKILL would make that index
// permanently refuse every later scan of the root — a safety property that
// turns into an outage is not a safety property.
func (c *Catalog) BeginScan(ctx context.Context, rootID string, now time.Time) (string, error) {
	id := uuid.Must(uuid.NewV7()).String()
	stamp := now.Format(timestampFormat)

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE scan_runs
			SET state = 'cancelled', finished_at = ?, updated_at = ?,
			    last_error = coalesce(last_error, 'abandoned — no process was still running this scan')
			WHERE root_id = ? AND state = 'running'`, stamp, stamp, rootID)
		if err != nil {
			return fmt.Errorf("catalog: closing abandoned scans of root %s: %w", rootID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			c.log.Warn("closed an abandoned scan run before starting a new one",
				"root_id", rootID, "abandoned", n)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scan_runs (id, root_id, state, started_at, updated_at)
			VALUES (?, ?, 'running', ?, ?)`, id, rootID, stamp, stamp); err != nil {
			return fmt.Errorf("catalog: opening a scan run for root %s: %w", rootID, err)
		}

		ev, err := c.events.EmitTx(ctx, tx, events.TypeScanProgress, "scan_run", id, map[string]any{
			"root_id": rootID,
			"state":   "running",
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
	return id, nil
}

// RecordProgress writes the fingerprints observed since the last call, updates
// the run's counters and emits system.scan.progress — all in one transaction.
//
// One transaction is the point. The fingerprint cache is the record of what the
// scan has already dealt with and the run row is the record of how far it got;
// written separately, a crash between them leaves a scan that says it handled
// 4 000 files and a cache that remembers 3 800, and the difference is silently
// re-hashed or silently skipped depending on which way round it fell.
func (c *Catalog) RecordProgress(ctx context.Context, run scanner.Run, seen []scanner.Fingerprint, now time.Time) error {
	stamp := now.Format(timestampFormat)
	var pending []events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, fp := range seen {
			// An upsert rather than an insert: a file that changed already has
			// a row, and this is the write that says "this version of it is
			// accounted for".
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO scanned_files (root_id, path, size, mtime_ns, dev, inode, scanned_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (root_id, path) DO UPDATE SET
					size = excluded.size, mtime_ns = excluded.mtime_ns,
					dev = excluded.dev, inode = excluded.inode,
					scanned_at = excluded.scanned_at`,
				run.RootID, fp.RelPath, fp.Size, fp.MtimeNS, fp.Dev, fp.Inode, stamp); err != nil {
				return fmt.Errorf("catalog: caching the fingerprint of %s: %w", fp.RelPath, err)
			}
		}

		if err := updateRunCounters(ctx, tx, run, stamp); err != nil {
			return err
		}

		ev, err := c.events.EmitTx(ctx, tx, events.TypeScanProgress, "scan_run", run.ID,
			progressPayload(run, "running"))
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

// MarkVanished forgets cached paths that are gone and marks their assets
// missing.
//
// It never deletes a blob, and it never deletes an asset. Deletion is logical;
// bytes are reclaimed by a refcounted garbage collector after a grace window
// (ADR-0018). A scanner that unlinked bytes because a mount was not ready when
// it ran would be the most destructive component in the system.
func (c *Catalog) MarkVanished(ctx context.Context, run scanner.Run, gone []scanner.Vanished, now time.Time) (int64, error) {
	if len(gone) == 0 {
		return 0, nil
	}
	stamp := now.Format(timestampFormat)
	var (
		pending []events.Event
		marked  int64
	)

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, v := range gone {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM scanned_files WHERE root_id = ? AND path = ?`,
				run.RootID, v.RelPath); err != nil {
				return fmt.Errorf("catalog: forgetting the fingerprint of %s: %w", v.RelPath, err)
			}

			var assetID string
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM assets
				WHERE library_id = ? AND source_path = ? AND source_class = 'managed'
				  AND missing_since IS NULL`,
				run.LibraryID, v.SourcePath).Scan(&assetID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// The file was scanned but never ingested, or was already
				// marked. Forgetting the fingerprint is the whole of the work.
				continue
			case err != nil:
				return fmt.Errorf("catalog: looking for the asset at %s: %w", v.SourcePath, err)
			}

			if _, err := tx.ExecContext(ctx,
				`UPDATE assets SET missing_since = ?, updated_at = ? WHERE id = ?`,
				stamp, stamp, assetID); err != nil {
				return fmt.Errorf("catalog: marking asset %s missing: %w", assetID, err)
			}
			marked++

			ev, err := c.events.EmitTx(ctx, tx, events.TypeAssetMissing, "asset", assetID, map[string]any{
				"library_id":  run.LibraryID,
				"root_id":     run.RootID,
				"scan_run_id": run.ID,
				"source_path": v.SourcePath,
				// Said explicitly because it is the property that matters: the
				// bytes are still under management and still replicated. Only
				// the path they were found at is gone.
				"blob_retained": true,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	c.events.Publish(pending...)
	if marked > 0 {
		c.log.Info("marked assets missing after a scan",
			"root_id", run.RootID, "scan_run_id", run.ID, "assets", marked, "paths", len(gone))
	}
	return marked, nil
}

// FinishScan closes the run and emits a final progress event.
func (c *Catalog) FinishScan(ctx context.Context, run scanner.Run, state, failure string, now time.Time) error {
	stamp := now.Format(timestampFormat)
	var pending []events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := updateRunCounters(ctx, tx, run, stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE scan_runs SET state = ?, finished_at = ?, updated_at = ?, last_error = ?
			WHERE id = ?`, state, stamp, stamp, nullString(failure), run.ID); err != nil {
			return fmt.Errorf("catalog: closing scan run %s: %w", run.ID, err)
		}
		payload := progressPayload(run, state)
		if failure != "" {
			payload["error"] = failure
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeScanProgress, "scan_run", run.ID, payload)
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

func updateRunCounters(ctx context.Context, tx *sql.Tx, run scanner.Run, stamp string) error {
	p := run.Progress
	if _, err := tx.ExecContext(ctx, `
		UPDATE scan_runs SET files_seen = ?, files_enqueued = ?, files_unchanged = ?,
			files_skipped = ?, files_missing = ?, errors = ?, bytes_seen = ?, updated_at = ?
		WHERE id = ?`,
		p.FilesSeen, p.FilesEnqueued, p.FilesUnchanged, p.FilesSkipped, p.FilesMissing,
		p.Errors, p.BytesSeen, stamp, run.ID); err != nil {
		return fmt.Errorf("catalog: updating scan run %s: %w", run.ID, err)
	}
	return nil
}

func progressPayload(run scanner.Run, state string) map[string]any {
	p := run.Progress
	return map[string]any{
		"root_id":         run.RootID,
		"library_id":      run.LibraryID,
		"state":           state,
		"files_seen":      p.FilesSeen,
		"files_enqueued":  p.FilesEnqueued,
		"files_unchanged": p.FilesUnchanged,
		"files_skipped":   p.FilesSkipped,
		"files_missing":   p.FilesMissing,
		"errors":          p.Errors,
		"bytes_seen":      p.BytesSeen,
	}
}
