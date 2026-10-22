package catalog

import (
	"context"
	"fmt"
)

// Purging a media node's legacy vault pins (#823, #779).
//
// Before the vault moved to the personal profile (ADR-0107, #754), a media node
// held vault blobs and the placement pins that kept them. Since #784 a media
// node mounts no vault routes, so no device can unpin those blobs any more, and
// without an unpin no release is ever recorded: GC's last-copy rule keeps the
// bytes forever. The purge is the operator's lever. It removes every placement
// pin through [Catalog.UnpinPlacement], so release intent is recorded in the same
// transaction as each removal, exactly as a device's unpin would have.
//
// Every pin on a media node is legacy vault: nothing on the media profile writes
// one any more. Library media is never released (recordReleaseIfUnwanted refuses
// a blob an asset or scanned library file reaches), so a stray pin on library
// bytes is removed and the bytes stay.

// PinPurge summarises the placement pins a purge covers.
type PinPurge struct {
	// Pins is how many placement pins there are (before a purge) or were
	// removed (after one).
	Pins int `json:"pins"`
	// Blobs is how many distinct blobs those pins hold.
	Blobs int `json:"blobs"`
	// Bytes is the total size of those blobs that this node tracks. A pin
	// recorded before its bytes arrived has no blobs row and adds nothing.
	Bytes int64 `json:"bytes"`
	// Releasable is how many of those blobs nothing else wants once the pins
	// go: no taken placement, no asset and no scanned library file. After a
	// purge it is how many were released.
	Releasable int `json:"releasable"`
	// StillWanted is the rest: blobs a taken placement, an asset or a scanned
	// library file still reaches, which the purge unpins but does not release.
	StillWanted int `json:"still_wanted"`
}

// PlacementPinPurgePreview is what [Catalog.PurgePlacementPins] would do now,
// without changing anything.
func (c *Catalog) PlacementPinPurgePreview(ctx context.Context) (PinPurge, error) {
	var p PinPurge
	err := c.db.Reader().QueryRowContext(ctx, `
		WITH pinned AS (SELECT DISTINCT blob_hash FROM placement_pins)
		SELECT (SELECT COUNT(*) FROM placement_pins),
		       (SELECT COUNT(*) FROM pinned),
		       (SELECT COALESCE(SUM(b.size), 0) FROM pinned p JOIN blobs b ON b.hash = p.blob_hash),
		       (SELECT COUNT(*) FROM pinned p
		         WHERE NOT EXISTS (SELECT 1 FROM peer_placements pp WHERE pp.blob_hash = p.blob_hash)
		           AND NOT EXISTS (SELECT 1 FROM assets a WHERE a.blob_hash = p.blob_hash)
		           AND NOT EXISTS (SELECT 1 FROM scanned_files s WHERE s.blob_hash = p.blob_hash))`,
	).Scan(&p.Pins, &p.Blobs, &p.Bytes, &p.Releasable)
	if err != nil {
		return PinPurge{}, fmt.Errorf("catalog: summarising the placement pins: %w", err)
	}
	p.StillWanted = p.Blobs - p.Releasable
	return p, nil
}

// PurgePlacementPins removes every placement pin, one [Catalog.UnpinPlacement]
// at a time, so each blob nothing else wants records release intent in the
// transaction that removed its last pin. It returns what it removed and how
// many of those blobs ended up released.
//
// It is a media-node operation (#823): on the personal profile the pins are a
// device's live retention decisions, and the caller must refuse there. A pin
// written while the purge runs is not removed; a second run is a no-op when
// there are none.
func (c *Catalog) PurgePlacementPins(ctx context.Context) (PinPurge, error) {
	before, err := c.PlacementPinPurgePreview(ctx)
	if err != nil {
		return PinPurge{}, err
	}
	pins, err := c.AllPlacementPins(ctx)
	if err != nil {
		return PinPurge{}, err
	}
	blobs := map[string]bool{}
	for _, p := range pins {
		if err := ctx.Err(); err != nil {
			return PinPurge{}, err
		}
		if err := c.UnpinPlacement(ctx, p.BlobHash, p.PeerID); err != nil {
			return PinPurge{}, fmt.Errorf("catalog: purging the pin of %s on peer %s: %w", p.BlobHash, p.PeerID, err)
		}
		blobs[p.BlobHash] = true
	}
	out := PinPurge{Pins: len(pins), Blobs: len(blobs), Bytes: before.Bytes}
	for hash := range blobs {
		var released bool
		if err := c.db.Reader().QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM blob_releases WHERE blob_hash = ?)`, hash).Scan(&released); err != nil {
			return PinPurge{}, fmt.Errorf("catalog: reading the release of %s: %w", hash, err)
		}
		if released {
			out.Releasable++
		}
	}
	out.StillWanted = out.Blobs - out.Releasable
	return out, nil
}
