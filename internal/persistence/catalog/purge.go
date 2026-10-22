package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Purging a media node's legacy vault data (#823, #779).
//
// Before the vault moved to the personal profile (ADR-0107, #754), a media node
// held vault blobs, the placement pins that kept them and the placements it took
// from a sibling (ADR-0114). Since #784 a media node mounts no vault routes and
// runs no placement exchange, so no device can unpin those blobs and no sibling
// can withdraw them any more. Without either, no release is ever recorded and
// GC's last-copy rule keeps the bytes forever. The purge is the operator's
// lever: it removes every placement pin and every taken placement, and records
// release intent in the same transaction as each blob's removal, exactly as a
// device's unpin or a sibling's withdrawal would have.
//
// Every pin and taken placement on a media node is legacy vault: nothing on the
// media profile writes one any more. Library media is never released
// (recordReleaseIfUnwanted refuses a blob an asset or scanned library file
// reaches), so a stray pin on library bytes is removed and the bytes stay.
//
// A pre-#659 vault upload left bytes and a pin but no blobs row. GC spares such
// bytes only while the pin stands and judges an untracked file by its own age,
// so removing the pin alone would let the next sweep unlink it at once, not
// after the grace window a release promises. The purge adopts those bytes first
// (a blobs row from this node's own Stat), so their release is honoured after
// the window like any other.

// HeldSize reports whether this node's store holds the bytes for hash, and
// their size. The purge uses it to adopt pinned bytes that have no blobs row.
type HeldSize func(ctx context.Context, hash string) (size int64, held bool, err error)

// PinPurge summarises the legacy vault data a purge covers.
type PinPurge struct {
	// Pins is how many placement pins there are (before a purge) or were
	// removed (after one).
	Pins int `json:"pins"`
	// Taken is how many placements taken from a sibling there are, or were
	// retired.
	Taken int `json:"taken"`
	// Blobs is how many distinct blobs those pins and placements hold.
	Blobs int `json:"blobs"`
	// Bytes is the total size of those blobs that this node tracks or holds. A
	// pin recorded before its bytes arrived has neither and adds nothing.
	Bytes int64 `json:"bytes"`
	// Adopted is how many of those blobs this node holds with no blobs row (a
	// pre-#659 upload), and so would get, or got, one before their release.
	Adopted int `json:"adopted"`
	// Releasable is how many of those blobs nothing else wants once the pins
	// and placements go: no asset and no scanned library file. After a purge it
	// is how many were released.
	Releasable int `json:"releasable"`
	// StillWanted is the rest: library media an asset or a scanned library file
	// reaches, which the purge unpins but never releases.
	StillWanted int `json:"still_wanted"`
}

// purgeBlob is one blob the purge covers, as the catalog sees it now.
type purgeBlob struct {
	hash    string
	size    sql.NullInt64 // its blobs row's size; NULL when there is no row
	library bool          // an asset or a scanned library file reaches it
}

// errNothingPurged rolls back a blob's transaction when another writer removed
// its pins and placements first, so a speculative adoption is not kept.
var errNothingPurged = errors.New("catalog: nothing left to purge")

// purgeSet reads every blob a placement pin or a taken placement holds, with
// the pin and placement counts.
func (c *Catalog) purgeSet(ctx context.Context) (blobs []purgeBlob, pins, taken int, err error) {
	r := c.db.Reader()
	if err := r.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM placement_pins), (SELECT COUNT(*) FROM peer_placements)`,
	).Scan(&pins, &taken); err != nil {
		return nil, 0, 0, fmt.Errorf("catalog: counting the placement pins: %w", err)
	}
	rows, err := r.QueryContext(ctx, `
		WITH held AS (SELECT blob_hash FROM placement_pins
		              UNION SELECT blob_hash FROM peer_placements)
		SELECT h.blob_hash, b.size,
		       EXISTS (SELECT 1 FROM assets a WHERE a.blob_hash = h.blob_hash)
		       OR EXISTS (SELECT 1 FROM scanned_files s WHERE s.blob_hash = h.blob_hash)
		  FROM held h LEFT JOIN blobs b ON b.hash = h.blob_hash
		 ORDER BY h.blob_hash`)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("catalog: reading the pinned blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b purgeBlob
		if err := rows.Scan(&b.hash, &b.size, &b.library); err != nil {
			return nil, 0, 0, err
		}
		blobs = append(blobs, b)
	}
	return blobs, pins, taken, rows.Err()
}

// PlacementPinPurgePreview is what [Catalog.PurgePlacementPins] would do now,
// without changing anything. held may be nil, in which case bytes with no
// blobs row are neither counted nor adopted.
func (c *Catalog) PlacementPinPurgePreview(ctx context.Context, held HeldSize) (PinPurge, error) {
	blobs, pins, taken, err := c.purgeSet(ctx)
	if err != nil {
		return PinPurge{}, err
	}
	p := PinPurge{Pins: pins, Taken: taken, Blobs: len(blobs)}
	for _, b := range blobs {
		switch {
		case b.size.Valid:
			p.Bytes += b.size.Int64
		case held != nil:
			size, ok, err := held(ctx, b.hash)
			if err != nil {
				return PinPurge{}, fmt.Errorf("catalog: checking the bytes of %s: %w", b.hash, err)
			}
			if ok {
				p.Adopted++
				p.Bytes += size
			}
		}
		if !b.library {
			p.Releasable++
		}
	}
	p.StillWanted = p.Blobs - p.Releasable
	return p, nil
}

// PurgePlacementPins removes every placement pin and taken placement, one blob
// per transaction. Each transaction adopts held bytes that have no blobs row,
// then records release intent when nothing else wants the blob. It returns what
// it removed, adopted and released.
//
// It is a media-node operation (#823): on the personal profile the pins are a
// device's live retention decisions, and the caller must refuse there. A pin
// written while the purge runs is not removed; a second run is a no-op when
// there are none.
func (c *Catalog) PurgePlacementPins(ctx context.Context, held HeldSize) (PinPurge, error) {
	blobs, _, _, err := c.purgeSet(ctx)
	if err != nil {
		return PinPurge{}, err
	}
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return PinPurge{}, err
	}
	var out PinPurge
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return PinPurge{}, err
		}
		r, err := c.purgeBlob(ctx, b.hash, self, held)
		if err != nil {
			return PinPurge{}, err
		}
		if r.Pins+r.Taken == 0 {
			continue
		}
		out.Pins += r.Pins
		out.Taken += r.Taken
		out.Blobs++
		out.Bytes += r.Bytes
		out.Adopted += r.Adopted
		out.Releasable += r.Releasable
	}
	out.StillWanted = out.Blobs - out.Releasable
	return out, nil
}

// purgeBlob removes one blob's pins and taken placements in one transaction,
// adopting its bytes first when it has no row, and records its release when
// nothing else wants it.
func (c *Catalog) purgeBlob(ctx context.Context, hash, self string, held HeldSize) (PinPurge, error) {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var (
		out     PinPurge
		pending []events.Event
	)
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		out, pending = PinPurge{}, nil
		var size sql.NullInt64
		switch err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash = ?`, hash).Scan(&size); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("catalog: reading blob %s: %w", hash, err)
		}
		if size.Valid {
			out.Bytes = size.Int64
		} else if held != nil {
			n, ok, err := held(ctx, hash)
			if err != nil {
				return fmt.Errorf("catalog: checking the bytes of %s: %w", hash, err)
			}
			if ok {
				if _, err := adoptBlob(ctx, tx, hash, n, now); err != nil {
					return err
				}
				ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", hash, map[string]any{
					"size": n, "adopted": true,
				})
				if err != nil {
					return err
				}
				pending = append(pending, ev)
				out.Bytes, out.Adopted = n, 1
			}
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM placement_pins WHERE blob_hash = ?`, hash)
		if err != nil {
			return fmt.Errorf("catalog: removing the placement pins of %s: %w", hash, err)
		}
		pins, err := res.RowsAffected()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM peer_placements WHERE blob_hash = ?`, hash)
		if err != nil {
			return fmt.Errorf("catalog: retiring the taken placements of %s: %w", hash, err)
		}
		taken, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if pins+taken == 0 {
			return errNothingPurged
		}
		out.Pins, out.Taken = int(pins), int(taken)
		if taken > 0 {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeSyncPlacementsExchanged, "peer", self, map[string]any{
				"range": "blob", "blob_hash": hash, "added": 0, "removed": taken,
				"reason": "legacy vault purge on a media node",
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		cause := releaseUnpinned
		if pins == 0 {
			cause = releaseWithdrawn
		}
		evs, err := c.recordReleaseIfUnwanted(ctx, tx, hash, cause, "", now)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
		var released bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM blob_releases WHERE blob_hash = ?)`, hash).Scan(&released); err != nil {
			return fmt.Errorf("catalog: reading the release of %s: %w", hash, err)
		}
		if released {
			out.Releasable = 1
		}
		return nil
	})
	switch {
	case errors.Is(err, errNothingPurged):
		return PinPurge{}, nil
	case err != nil:
		return PinPurge{}, err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return out, nil
}
