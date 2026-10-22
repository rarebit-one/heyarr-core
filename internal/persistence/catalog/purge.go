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

// PurgePlacementPins removes every taken placement and every placement pin,
// adopting held bytes that have no blobs row and recording release intent for
// each blob nothing else wants in the transaction that removed the last thing
// holding it. It returns what it removed, adopted and released.
//
// The taken placements go first, in one transaction with one
// sync.placements_exchanged event per source (that event is per exchange, never
// per blob); a blob also pinned here is released when its pins go. The
// pins then go one blob per transaction, as a device's unpin would.
//
// It is a media-node operation (#823): on the personal profile the pins are a
// device's live retention decisions, and the caller must refuse there. A pin
// written while the purge runs is not removed; a second run is a no-op when
// there are none.
func (c *Catalog) PurgePlacementPins(ctx context.Context, held HeldSize) (PinPurge, error) {
	touched := map[string]bool{}
	var out PinPurge
	taken, adopted, err := c.retireTakenPlacements(ctx, held, touched)
	if err != nil {
		return PinPurge{}, err
	}
	out.Taken, out.Adopted = taken, adopted

	var pinned []string
	if err := c.collect(ctx, &pinned, `SELECT DISTINCT blob_hash FROM placement_pins ORDER BY blob_hash`); err != nil {
		return PinPurge{}, err
	}
	for _, hash := range pinned {
		if err := ctx.Err(); err != nil {
			return PinPurge{}, err
		}
		pins, adopted, err := c.unpinBlob(ctx, hash, held)
		if err != nil {
			return PinPurge{}, err
		}
		if pins > 0 {
			out.Pins += pins
			out.Adopted += adopted
			touched[hash] = true
		}
	}

	r := c.db.Reader()
	for hash := range touched {
		var (
			size     sql.NullInt64
			released bool
		)
		if err := r.QueryRowContext(ctx, `
			SELECT (SELECT size FROM blobs WHERE hash = ?),
			       EXISTS (SELECT 1 FROM blob_releases WHERE blob_hash = ?)`, hash, hash,
		).Scan(&size, &released); err != nil {
			return PinPurge{}, fmt.Errorf("catalog: reading the release of %s: %w", hash, err)
		}
		out.Blobs++
		out.Bytes += size.Int64
		if released {
			out.Releasable++
		}
	}
	out.StillWanted = out.Blobs - out.Releasable
	return out, nil
}

// retireTakenPlacements removes every placement taken from a sibling in one
// transaction, adopting held bytes without a row and releasing each blob no
// pin, asset or scanned library file still wants (cause withdrawn). Each source
// gets one sync.placements_exchanged event for what was retired from it, as a
// withdrawing answer from that sibling would have. It adds each blob it touched
// to touched and reports how many placements went and how many blobs it
// adopted.
func (c *Catalog) retireTakenPlacements(
	ctx context.Context, held HeldSize, touched map[string]bool,
) (taken, adopted int, err error) {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var (
		pending []events.Event
		hashes  []string
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		taken, adopted, pending = 0, 0, nil
		bySource, err := takenBySource(ctx, tx)
		if err != nil {
			return err
		}
		if hashes, err = takenHashes(ctx, tx); err != nil {
			return err
		}
		for _, hash := range hashes {
			evs, ok, err := c.adoptHeld(ctx, tx, hash, held, now)
			if err != nil {
				return err
			}
			if ok {
				adopted++
			}
			pending = append(pending, evs...)
			res, err := tx.ExecContext(ctx, `DELETE FROM peer_placements WHERE blob_hash = ?`, hash)
			if err != nil {
				return fmt.Errorf("catalog: retiring the taken placements of %s: %w", hash, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			taken += int(n)
			evs, err = c.recordReleaseIfUnwanted(ctx, tx, hash, releaseWithdrawn, "", now)
			if err != nil {
				return err
			}
			pending = append(pending, evs...)
		}
		for _, src := range bySource {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeSyncPlacementsExchanged, "peer", src.peer, map[string]any{
				"range": "all", "added": 0, "removed": src.count,
				"reason": "legacy vault purge on a media node",
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	for _, hash := range hashes {
		touched[hash] = true
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return taken, adopted, nil
}

// sourceCount is how many placements were taken from one sibling.
type sourceCount struct {
	peer  string
	count int
}

// takenBySource counts the taken placements per source, inside tx.
func takenBySource(ctx context.Context, tx *sql.Tx) ([]sourceCount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_peer_id, COUNT(*) FROM peer_placements
		 GROUP BY source_peer_id ORDER BY source_peer_id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: counting the taken placements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []sourceCount
	for rows.Next() {
		var s sourceCount
		if err := rows.Scan(&s.peer, &s.count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// takenHashes is every blob a taken placement holds, inside tx.
func takenHashes(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT blob_hash FROM peer_placements ORDER BY blob_hash`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the taken placements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// unpinBlob removes one blob's placement pins in one transaction, adopting its
// bytes first when it has no row, and records its release when nothing else
// wants it. It reports how many pins went and whether it adopted the blob.
func (c *Catalog) unpinBlob(ctx context.Context, hash string, held HeldSize) (pins, adopted int, err error) {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		pins, adopted, pending = 0, 0, nil
		evs, ok, err := c.adoptHeld(ctx, tx, hash, held, now)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
		res, err := tx.ExecContext(ctx, `DELETE FROM placement_pins WHERE blob_hash = ?`, hash)
		if err != nil {
			return fmt.Errorf("catalog: removing the placement pins of %s: %w", hash, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Unpinned since the purge read it: keep no speculative adoption.
			return errNothingPurged
		}
		pins = int(n)
		if ok {
			adopted = 1
		}
		evs, err = c.recordReleaseIfUnwanted(ctx, tx, hash, releaseUnpinned, "", now)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
		return nil
	})
	switch {
	case errors.Is(err, errNothingPurged):
		return 0, 0, nil
	case err != nil:
		return 0, 0, err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return pins, adopted, nil
}

// adoptHeld records a blobs row for bytes this node holds under hash when it
// has none, so a release of them is honoured after the grace window rather
// than their being swept as untracked. It reports whether it adopted them.
func (c *Catalog) adoptHeld(
	ctx context.Context, tx *sql.Tx, hash string, held HeldSize, now string,
) ([]events.Event, bool, error) {
	if held == nil {
		return nil, false, nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE hash = ?)`, hash).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("catalog: reading blob %s: %w", hash, err)
	}
	if exists {
		return nil, false, nil
	}
	size, ok, err := held(ctx, hash)
	if err != nil {
		return nil, false, fmt.Errorf("catalog: checking the bytes of %s: %w", hash, err)
	}
	if !ok {
		return nil, false, nil
	}
	if _, err := adoptBlob(ctx, tx, hash, size, now); err != nil {
		return nil, false, err
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", hash, map[string]any{
		"size": size, "adopted": true,
	})
	if err != nil {
		return nil, false, err
	}
	return []events.Event{ev}, true, nil
}
