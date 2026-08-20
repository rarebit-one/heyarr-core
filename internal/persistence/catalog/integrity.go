package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

var _ integrity.Catalog = (*Catalog)(nil)

// Blobs lists every blob with its reference count and grace-window mark.
//
// The reference count is computed rather than stored. A denormalised counter is
// faster and is exactly the kind of thing that drifts silently: one missed
// decrement and garbage collection deletes content somebody is still using,
// irreversibly and with no error anywhere. The count comes from the assets
// table, which is the only thing that can be right, and assets_by_blob makes it
// an index scan.
func (c *Catalog) Blobs(ctx context.Context) ([]integrity.Blob, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT b.hash, b.size, b.unreferenced_since,
		       (SELECT count(*) FROM assets a WHERE a.blob_hash = b.hash)
		FROM blobs b
		ORDER BY b.hash`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []integrity.Blob
	for rows.Next() {
		b, err := scanBlob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: listing blobs: %w", err)
	}
	return out, nil
}

// Blob reads one blob's integrity record.
func (c *Catalog) Blob(ctx context.Context, h hashing.Hash) (integrity.Blob, error) {
	row := c.db.Reader().QueryRowContext(ctx, `
		SELECT b.hash, b.size, b.unreferenced_since,
		       (SELECT count(*) FROM assets a WHERE a.blob_hash = b.hash)
		FROM blobs b WHERE b.hash = ?`, h.String())
	b, err := scanBlob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return integrity.Blob{}, fmt.Errorf("%w: %s", integrity.ErrUnknownBlob, h)
	}
	return b, err
}

// rowScanner is what a *sql.Row and a *sql.Rows have in common. It is not
// called `scanner`: this package imports internal/scanner (M1-12), and a local
// type of that name shadows the package for the whole file.
type rowScanner interface{ Scan(dest ...any) error }

func scanBlob(s rowScanner) (integrity.Blob, error) {
	var (
		raw, since sql.NullString
		size       int64
		refs       int
	)
	if err := s.Scan(&raw, &size, &since, &refs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return integrity.Blob{}, err
		}
		return integrity.Blob{}, fmt.Errorf("catalog: reading a blob: %w", err)
	}
	h, err := hashing.Parse(raw.String)
	if err != nil {
		// The schema's CHECK constraint makes this unreachable through any
		// path Heyarr writes. If it happens, someone edited the database by
		// hand, and inventing a hash to carry on with would be worse.
		return integrity.Blob{}, fmt.Errorf("catalog: blob %q is not a valid hash: %w", raw.String, err)
	}
	b := integrity.Blob{Hash: h, Size: size, References: refs}
	if since.Valid {
		if t, err := time.Parse(timestampFormat, since.String); err == nil {
			b.UnreferencedSince = t
		}
	}
	return b, nil
}

// Known reports which of these hashes still have a blobs row.
func (c *Catalog) Known(ctx context.Context, hashes []hashing.Hash) (map[string]bool, error) {
	out := make(map[string]bool, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	// One statement per hash rather than a generated IN list: the caller is a
	// garbage collector about to unlink bytes, the set is small by the time it
	// gets here, and an assembled IN clause is the shape that turns into an
	// injection the day somebody reuses this helper for a wider input.
	stmt, err := c.db.Reader().PrepareContext(ctx, `SELECT 1 FROM blobs WHERE hash = ?`)
	if err != nil {
		return nil, fmt.Errorf("catalog: checking known blobs: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, h := range hashes {
		var one int
		err := stmt.QueryRowContext(ctx, h.String()).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out[h.String()] = false
		case err != nil:
			return nil, fmt.Errorf("catalog: checking known blob %s: %w", h, err)
		default:
			out[h.String()] = true
		}
	}
	return out, nil
}

// MarkVerified stamps a successful verification on this peer's replica.
//
// A replica that was corrupt or missing and now verifies is a state transition
// and emits replica.present. One that was already present is not: a deep fsck
// over a healthy library would otherwise emit one event per blob, which is a
// hundred thousand records of nothing having happened.
func (c *Catalog) MarkVerified(ctx context.Context, h hashing.Hash, at time.Time) error {
	peerID, err := c.SelfPeer(ctx)
	if err != nil {
		return err
	}
	stamp := at.UTC().Format(timestampFormat)

	var pending []events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		previous, size, err := replicaState(ctx, tx, h.String(), peerID)
		if err != nil {
			return err
		}
		if size == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash = ?`, h.String()).
				Scan(&size); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("catalog: reading blob size for %s: %w", h, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				verified_at = excluded.verified_at, updated_at = excluded.updated_at`,
			h.String(), peerID, size, stamp, stamp); err != nil {
			return fmt.Errorf("catalog: stamping verification of %s: %w", h, err)
		}
		if previous == "present" {
			return nil
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaPresent, "blob", h.String(), map[string]any{
			"peer_id":       peerID,
			"bytes_present": size,
			"previous":      previous,
			"verified":      true,
		})
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

// MarkCorrupt records a failed verification and appends the quarantine ledger
// entry.
//
// The ledger is the difference between quarantine and a shrug. ADR-0018 keeps
// the bytes because they are evidence — frequently evidence that an external
// tool rewrote the operator's own file through a shared inode (#43) — and
// evidence nobody can date, explain or locate is not evidence.
func (c *Catalog) MarkCorrupt(ctx context.Context, corruption integrity.Corruption, at time.Time) error {
	peerID, err := c.SelfPeer(ctx)
	if err != nil {
		return err
	}
	stamp := at.UTC().Format(timestampFormat)
	hash := corruption.Hash.String()

	var pending []events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			VALUES (?, ?, 'corrupt', 0, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'corrupt', bytes_present = 0, updated_at = excluded.updated_at`,
			hash, peerID, stamp); err != nil {
			return fmt.Errorf("catalog: marking %s corrupt: %w", hash, err)
		}

		var actual any
		if !corruption.Actual.IsZero() {
			actual = corruption.Actual.String()
		}
		reason := corruption.Detail
		if reason == "" {
			reason = "hash mismatch on verification"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quarantine (id, blob_hash, peer_id, path, reason, actual_hash, size, detail, quarantined_at)
			VALUES (?, ?, ?, ?, 'hash_mismatch', ?, ?, ?, ?)`,
			uuid.Must(uuid.NewV7()).String(), hash, peerID, corruption.Path,
			actual, corruption.Size, reason, stamp); err != nil {
			return fmt.Errorf("catalog: recording quarantine of %s: %w", hash, err)
		}

		ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaCorrupt, "blob", hash, map[string]any{
			"peer_id":         peerID,
			"expected":        hash,
			"actual":          corruption.Actual.String(),
			"size":            corruption.Size,
			"quarantine_path": corruption.Path,
			"detail":          reason,
		})
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
	c.log.Warn("blob quarantined", "blob_hash", hash, "actual", corruption.Actual.String(),
		"path", corruption.Path, "detail", corruption.Detail)
	return nil
}

// MarkMissing records that this peer does not hold a blob's bytes, and marks
// the assets that referenced them missing.
//
// The asset half matters more than the replica half: replicas.state is what
// Milestone 4's placement reads, but assets.missing_since is what tells a user
// the film they think they own is not there.
func (c *Catalog) MarkMissing(ctx context.Context, h hashing.Hash, at time.Time) error {
	peerID, err := c.SelfPeer(ctx)
	if err != nil {
		return err
	}
	stamp := at.UTC().Format(timestampFormat)
	hash := h.String()

	var pending []events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		previous, _, err := replicaState(ctx, tx, hash, peerID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			VALUES (?, ?, 'missing', 0, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'missing', bytes_present = 0, updated_at = excluded.updated_at`,
			hash, peerID, stamp); err != nil {
			return fmt.Errorf("catalog: marking replica of %s missing: %w", hash, err)
		}

		newlyMissing, err := assetsNewlyMissing(ctx, tx, hash)
		if err != nil {
			return err
		}
		if len(newlyMissing) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE assets SET missing_since = ?, updated_at = ? WHERE blob_hash = ? AND missing_since IS NULL`,
				stamp, stamp, hash); err != nil {
				return fmt.Errorf("catalog: marking assets of %s missing: %w", hash, err)
			}
		}

		if previous != "missing" {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaMissing, "blob", hash, map[string]any{
				"peer_id":  peerID,
				"previous": previous,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		for _, assetID := range newlyMissing {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeAssetMissing, "asset", assetID, map[string]any{
				"blob_hash": hash,
				"peer_id":   peerID,
				"reason":    "bytes are not in the content store",
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// MarkUnreferenced starts the grace window for blobs with no referencing asset.
//
// It deliberately writes nothing but a timestamp. This is the pass that decides
// what garbage collection will be allowed to delete a week from now, and the
// only safe thing for it to do today is say so.
func (c *Catalog) MarkUnreferenced(ctx context.Context, hashes []hashing.Hash, at time.Time) error {
	if len(hashes) == 0 {
		return nil
	}
	stamp := at.UTC().Format(timestampFormat)
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, h := range hashes {
			// The WHERE clause is what makes a re-run idempotent: a second
			// sweep must not restart a window that is already half spent, and
			// must not shorten one either.
			if _, err := tx.ExecContext(ctx,
				`UPDATE blobs SET unreferenced_since = ? WHERE hash = ? AND unreferenced_since IS NULL`,
				stamp, h.String()); err != nil {
				return fmt.Errorf("catalog: marking %s unreferenced: %w", h, err)
			}
		}
		return nil
	})
}

// ClearUnreferenced ends the grace window for blobs that regained a reference.
func (c *Catalog) ClearUnreferenced(ctx context.Context, hashes []hashing.Hash) error {
	if len(hashes) == 0 {
		return nil
	}
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx,
				`UPDATE blobs SET unreferenced_since = NULL WHERE hash = ?`, h.String()); err != nil {
				return fmt.Errorf("catalog: clearing the grace window on %s: %w", h, err)
			}
		}
		return nil
	})
}

// Reclaim removes the catalog's record of a blob and emits blob.reclaimed.
//
// The DELETE is the refcount check, not a consequence of it. assets.blob_hash
// is ON DELETE RESTRICT, so a blob any asset still points at cannot be removed
// here however wrong the caller's arithmetic was — the database refuses and the
// bytes stay. That redundancy is deliberate: every other bug in this system
// costs a re-run, and this one costs the operator their content.
func (c *Catalog) Reclaim(ctx context.Context, h hashing.Hash, size int64, tracked bool, at time.Time) error {
	hash := h.String()
	stamp := at.UTC().Format(timestampFormat)

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if tracked {
			res, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, hash)
			if err != nil {
				return fmt.Errorf("catalog: refusing to reclaim %s — the database rejected the "+
					"delete, which for this table means an asset still references it "+
					"(assets.blob_hash is ON DELETE RESTRICT): %w", hash, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("catalog: reclaiming %s: %w", hash, err)
			}
			if n == 0 {
				// Already gone. Another sweep got there first, which is an
				// ordinary outcome for an idempotent job (ADR-0008).
				return nil
			}
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobReclaimed, "blob", hash, map[string]any{
			"size":         size,
			"tracked":      tracked,
			"reclaimed_at": stamp,
		})
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

// replicaState reads this peer's replica row, reporting "" when there is none.
func replicaState(ctx context.Context, tx *sql.Tx, hash, peerID string) (string, int64, error) {
	var (
		state string
		bytes int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT state, bytes_present FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		hash, peerID).Scan(&state, &bytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, nil
	case err != nil:
		return "", 0, fmt.Errorf("catalog: reading the replica of %s: %w", hash, err)
	}
	return state, bytes, nil
}

// assetsNewlyMissing lists the assets that would transition to missing.
func assetsNewlyMissing(ctx context.Context, tx *sql.Tx, hash string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM assets WHERE blob_hash = ? AND missing_since IS NULL`, hash)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading assets of %s: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: reading assets of %s: %w", hash, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading assets of %s: %w", hash, err)
	}
	return out, nil
}
