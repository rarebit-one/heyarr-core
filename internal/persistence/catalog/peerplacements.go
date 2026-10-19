package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// Two-site placement (ADR-0114). A device pins a vault blob at the site it
// talks to; the other site learns which blobs to keep by ASKING, and keeps what
// it took in peer_placements (migration 00065). This file is both halves:
//
//   - the source's: OfferPlacements, what this node offers an asking sibling;
//   - the destination's: TakenPlacements and ReplacePlacements, what this node
//     took from a source and the single writer that replaces it per range.
//
// The planner unions peer_placements in as self-pins (convergence.go), and GC
// counts them as references (integrity.go), so a taken placement is retained
// and pulled exactly like a self-pin. It is never offered on: see
// OfferPlacements.

var _ placementsync.LocalStore = (*Catalog)(nil)

// OfferPlacements is what this node offers the asking peer inside a range: the
// blobs pinned to the asker by a device, or, when the asker is a configured
// sibling, pinned here (an upload's self-pin), each with whether this node holds
// its bytes right now.
//
// Only placement_pins are read, never peer_placements: a blob this node took
// from a sibling is not this node's to offer, or two siblings would keep each
// other's pins alive after the origin dropped it (ADR-0114).
//
// A pinned blob this node no longer holds is still offered, with Held false: the
// pin is what the placement follows, and the asker's copy may be the only good
// one left. Only a blob with no `blobs` row is left out, since there is no size
// to offer; its pin heals once the row is back (#658).
func (c *Catalog) OfferPlacements(
	ctx context.Context, asker string, sibling bool, r placementsync.Range,
) ([]placementsync.Entry, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return nil, err
	}
	lo, hi := r.Bounds()
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT b.hash, b.size,
		       EXISTS (SELECT 1 FROM replicas mine
		                WHERE mine.blob_hash = b.hash AND mine.peer_id = ? AND mine.state = 'present')
		  FROM blobs b
		 WHERE b.hash >= ? AND (? = '' OR b.hash < ?)
		   AND (EXISTS (SELECT 1 FROM placement_pins p WHERE p.blob_hash = b.hash AND p.peer_id = ?)
		        OR (? AND EXISTS (SELECT 1 FROM placement_pins p WHERE p.blob_hash = b.hash AND p.peer_id = ?)))
		 ORDER BY b.hash`,
		self, lo, hi, hi, asker, sibling, self)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the placements offered to %s: %w", asker, err)
	}
	defer func() { _ = rows.Close() }()
	out := []placementsync.Entry{}
	for rows.Next() {
		var e placementsync.Entry
		if err := rows.Scan(&e.BlobHash, &e.Size, &e.Held); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TakenPlacements is what this node took from source inside a range, in hash
// order, as the source last offered it.
func (c *Catalog) TakenPlacements(
	ctx context.Context, source string, r placementsync.Range,
) ([]placementsync.Entry, error) {
	lo, hi := r.Bounds()
	return c.entries(ctx, `
		SELECT blob_hash, size, source_held FROM peer_placements
		 WHERE source_peer_id = ? AND blob_hash >= ? AND (? = '' OR blob_hash < ?)
		 ORDER BY blob_hash`, source, lo, hi, hi)
}

// HeldClaim is this node's complete claim about its own disk inside a range:
// every blob it holds `present` (ADR-0114). Sent to each sibling, which marks
// this node's replicas the claim omits as missing. The whole disk, not only what
// is pinned: a blob unpinned here is still held until GC takes it, and a claim
// that dropped it at unpin would tell the sibling this node had lost it.
func (c *Catalog) HeldClaim(ctx context.Context, r placementsync.Range) ([]placementsync.Entry, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return nil, err
	}
	lo, hi := r.Bounds()
	return c.entries(ctx, `
		SELECT b.hash, b.size, 1 FROM blobs b
		  JOIN replicas mine ON mine.blob_hash = b.hash AND mine.peer_id = ? AND mine.state = 'present'
		 WHERE b.hash >= ? AND (? = '' OR b.hash < ?)
		 ORDER BY b.hash`, self, lo, hi, hi)
}

// PresentReplicasIn is every blob inside a range peer is recorded as holding
// `present`: what a sibling's complete claim is reconciled against.
func (c *Catalog) PresentReplicasIn(ctx context.Context, peer string, r placementsync.Range) ([]string, error) {
	lo, hi := r.Bounds()
	var out []string
	err := c.collect(ctx, &out, `
		SELECT blob_hash FROM replicas
		 WHERE peer_id = ? AND state = 'present' AND blob_hash >= ? AND (? = '' OR blob_hash < ?)
		 ORDER BY blob_hash`, peer, lo, hi, hi)
	return out, err
}

// DropPlacementsFromOthers withdraws everything this node took from any peer
// not in keep: a sibling the operator stopped listing no longer decides what is
// kept here, and the bytes go only by GC's grace window (ADR-0018).
func (c *Catalog) DropPlacementsFromOthers(ctx context.Context, keep []string) (int, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return 0, err
	}
	keepJSON, err := json.Marshal(append([]string{}, keep...))
	if err != nil {
		return 0, err
	}
	var (
		dropped int
		pending []events.Event
	)
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		dropped, pending = 0, nil
		res, err := tx.ExecContext(ctx, `
			DELETE FROM peer_placements
			 WHERE source_peer_id NOT IN (SELECT value FROM json_each(?))`, string(keepJSON))
		if err != nil {
			return fmt.Errorf("catalog: dropping placements taken from unlisted peers: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		dropped = int(n)
		ev, err := c.events.EmitTx(ctx, tx, events.TypeSyncPlacementsExchanged, "peer", self, map[string]any{
			"range": "all", "added": 0, "removed": n, "reason": "source no longer a configured sibling",
		})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return dropped, nil
}

func (c *Catalog) entries(ctx context.Context, query string, args ...any) ([]placementsync.Entry, error) {
	rows, err := c.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading placements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []placementsync.Entry{}
	for rows.Next() {
		var e placementsync.Entry
		if err := rows.Scan(&e.BlobHash, &e.Size, &e.Held); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplacePlacements makes what this node took from source inside a range
// exactly entries, in one transaction (ADR-0114).
//
// Per new entry it records the placement, a `blobs` row adopted from the size
// hint (metadata only: the pull verifies the bytes, invariant 1), and a
// `present` replica for the source, which is the source's claim about its own
// disk and what lets this node's planner name it as the source to pull from.
// A withdrawn entry is a deleted placement row and nothing else: the bytes, if
// this node holds them, go only by the GC sweep and its grace window
// (invariant 8).
//
// Emits sync.placements_exchanged when anything changed, plus blob.created for
// an adopted row and replica.present for a replica that moved (invariant 7).
func (c *Catalog) ReplacePlacements(
	ctx context.Context, source string, r placementsync.Range, entries []placementsync.Entry,
) (added, removed int, err error) {
	if source == "" {
		return 0, 0, fmt.Errorf("catalog: a taken placement needs its source peer")
	}
	for _, e := range entries {
		if !r.Contains(e.BlobHash) {
			return 0, 0, fmt.Errorf("catalog: placement %s is outside the range %s", e.BlobHash, r)
		}
	}
	lo, hi := r.Bounds()
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	restated := 0
	err = c.db.InTx(ctx, func(tx *sql.Tx) error {
		added, removed, restated, pending = 0, 0, 0, nil
		if err := peerExists(ctx, tx, source); err != nil {
			return err
		}

		current, err := takenInTx(ctx, tx, source, lo, hi)
		if err != nil {
			return err
		}

		want := make(map[string]struct{}, len(entries))
		for _, e := range entries {
			want[e.BlobHash] = struct{}{}
			if was, had := current[e.BlobHash]; had {
				if was == e {
					continue
				}
				// Same placement, restated: its size or the source's hold
				// changed. Both are kept as offered, so the next digest
				// matches the source's.
				if _, err := tx.ExecContext(ctx, `
					UPDATE peer_placements SET size = ?, source_held = ? WHERE source_peer_id = ? AND blob_hash = ?`,
					e.Size, e.Held, source, e.BlobHash); err != nil {
					return fmt.Errorf("catalog: recording %s's restated placement of %s: %w", source, e.BlobHash, err)
				}
				restated++
				if was.Held == e.Held {
					continue
				}
				evs, err := c.recordSourceHold(ctx, tx, source, e, now)
				if err != nil {
					return err
				}
				pending = append(pending, evs...)
				continue
			}
			adopted, err := adoptBlob(ctx, tx, e.BlobHash, e.Size, now)
			if err != nil {
				return err
			}
			if adopted {
				ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", e.BlobHash, map[string]any{
					"size": e.Size, "vault": true, "placed_by": source,
				})
				if err != nil {
					return err
				}
				pending = append(pending, ev)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO peer_placements (source_peer_id, blob_hash, size, source_held, received_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (source_peer_id, blob_hash) DO NOTHING`,
				source, e.BlobHash, e.Size, e.Held, now); err != nil {
				return fmt.Errorf("catalog: recording a placement taken from %s: %w", source, err)
			}
			evs, err := c.recordSourceHold(ctx, tx, source, e, now)
			if err != nil {
				return err
			}
			pending = append(pending, evs...)
			added++
		}
		for h := range current {
			if _, keep := want[h]; keep {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM peer_placements WHERE source_peer_id = ? AND blob_hash = ?`, source, h); err != nil {
				return fmt.Errorf("catalog: withdrawing a placement taken from %s: %w", source, err)
			}
			removed++
		}

		if added > 0 || removed > 0 || restated > 0 {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeSyncPlacementsExchanged, "peer", source, map[string]any{
				"range": r.String(), "added": added, "removed": removed, "restated": restated,
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
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return added, removed, nil
}

// takenInTx is what was taken from source inside [lo, hi), with the source's
// recorded hold on each, read inside the replacing transaction.
func takenInTx(ctx context.Context, tx *sql.Tx, source, lo, hi string) (map[string]placementsync.Entry, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash, size, source_held FROM peer_placements
		 WHERE source_peer_id = ? AND blob_hash >= ? AND (? = '' OR blob_hash < ?)`,
		source, lo, hi, hi)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the placements taken from %s: %w", source, err)
	}
	defer func() { _ = rows.Close() }()
	current := map[string]placementsync.Entry{}
	for rows.Next() {
		var e placementsync.Entry
		if err := rows.Scan(&e.BlobHash, &e.Size, &e.Held); err != nil {
			return nil, err
		}
		current[e.BlobHash] = e
	}
	return current, rows.Err()
}

// recordSourceHold applies the source's claim about its own copy of an offered
// blob to its replica row: present (stamped as just confirmed, which is what
// GC's last-copy rule reads, ADR-0018) or, when it says it no longer holds it,
// moved from present to missing. A claim about the claimant only (ADR-0033).
func (c *Catalog) recordSourceHold(
	ctx context.Context, tx *sql.Tx, source string, e placementsync.Entry, now string,
) ([]events.Event, error) {
	if e.Held {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, reported_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				reported_at = excluded.reported_at, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present'`,
			e.BlobHash, source, e.Size, now, now)
		if err != nil {
			return nil, fmt.Errorf("catalog: recording %s's replica of %s: %w", source, e.BlobHash, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, err
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaPresent, "blob", e.BlobHash, map[string]any{
			"peer_id": source, "bytes_present": e.Size,
		})
		if err != nil {
			return nil, err
		}
		return []events.Event{ev}, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE replicas SET state = 'missing', bytes_present = 0, reported_at = ?, updated_at = ?
		 WHERE blob_hash = ? AND peer_id = ? AND state = 'present'`,
		now, now, e.BlobHash, source)
	if err != nil {
		return nil, fmt.Errorf("catalog: recording that %s no longer holds %s: %w", source, e.BlobHash, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, err
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaMissing, "blob", e.BlobHash, map[string]any{
		"peer_id": source,
	})
	if err != nil {
		return nil, err
	}
	return []events.Event{ev}, nil
}

// ConfirmOffered records that the source just restated, in an answer changed or
// not, that it holds every blob it offers this node inside a range with Held
// true (ADR-0114). Present replicas are stamped as just confirmed, which is the
// freshness GC's last-copy rule reads (ADR-0018); without it a taken blob's only
// other copy would read as stale an hour after it was taken. A replica marked
// missing meanwhile (a failed read, a full inventory) that the source still
// says it holds is moved back to present, with its event: the source is usable
// again, and the destination may plan another pull from it.
func (c *Catalog) ConfirmOffered(ctx context.Context, source string, r placementsync.Range) error {
	lo, hi := r.Bounds()
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		if _, err := tx.ExecContext(ctx, `
			UPDATE replicas SET reported_at = ?
			 WHERE peer_id = ? AND state = 'present'
			   AND blob_hash IN (SELECT blob_hash FROM peer_placements
			                      WHERE source_peer_id = ? AND source_held = 1
			                        AND blob_hash >= ? AND (? = '' OR blob_hash < ?))`,
			now, source, source, lo, hi, hi); err != nil {
			return fmt.Errorf("catalog: confirming %s's offered replicas: %w", source, err)
		}
		lost, err := reassertable(ctx, tx, source, lo, hi)
		if err != nil {
			return err
		}
		for _, e := range lost {
			evs, err := c.recordSourceHold(ctx, tx, source, e, now)
			if err != nil {
				return err
			}
			pending = append(pending, evs...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return nil
}

// reassertable is what source offers as held inside [lo, hi) whose replica this
// node does not record as present.
func reassertable(ctx context.Context, tx *sql.Tx, source, lo, hi string) ([]placementsync.Entry, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT pp.blob_hash, pp.size FROM peer_placements pp
		 WHERE pp.source_peer_id = ? AND pp.source_held = 1
		   AND pp.blob_hash >= ? AND (? = '' OR pp.blob_hash < ?)
		   AND NOT EXISTS (SELECT 1 FROM replicas r
		                    WHERE r.blob_hash = pp.blob_hash AND r.peer_id = pp.source_peer_id AND r.state = 'present')`,
		source, lo, hi, hi)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading %s's offered replicas: %w", source, err)
	}
	defer func() { _ = rows.Close() }()
	var out []placementsync.Entry
	for rows.Next() {
		e := placementsync.Entry{Held: true}
		if err := rows.Scan(&e.BlobHash, &e.Size); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PlacementStatus is where a blob is wanted and where it is known to be held:
// the read a client waits on before it treats a blob as safe at both sites
// (ADR-0114).
type PlacementStatus struct {
	// PinnedTo is every peer a placement names for the blob: device pins,
	// plus this node's configured siblings when the blob is pinned here.
	PinnedTo []string
	// HeldAt is every Full Peer known to hold the blob `present`: this node
	// by its own record, another peer only while its claim is fresh
	// (integrity.DefaultFreshness, the bound GC's last-copy rule uses).
	HeldAt []string
	// Sites is how many distinct sites HeldAt spans, by the peers' site
	// label (a peer with none is its own site): two copies at one site are
	// one failure domain.
	Sites int
	// TakenFrom is the sources this node took the blob from, if any.
	TakenFrom []string
}

// PlacementStatusOf reads a blob's placement status. siblings is this node's
// placement.siblings, which a self-pinned blob is offered to.
func (c *Catalog) PlacementStatusOf(ctx context.Context, blobHash string, siblings []string) (PlacementStatus, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return PlacementStatus{}, err
	}
	var st PlacementStatus
	pinned := map[string]struct{}{}
	selfPinned := false
	if err := c.collect(ctx, &st.PinnedTo, `SELECT peer_id FROM placement_pins WHERE blob_hash = ? ORDER BY peer_id`, blobHash); err != nil {
		return PlacementStatus{}, err
	}
	for _, p := range st.PinnedTo {
		pinned[p] = struct{}{}
		if p == self {
			selfPinned = true
		}
	}
	if err := c.collect(ctx, &st.TakenFrom,
		`SELECT source_peer_id FROM peer_placements WHERE blob_hash = ? ORDER BY source_peer_id`, blobHash); err != nil {
		return PlacementStatus{}, err
	}
	if len(st.TakenFrom) > 0 {
		// A placement taken from a sibling is this node's own desire.
		if _, ok := pinned[self]; !ok {
			pinned[self] = struct{}{}
			st.PinnedTo = append(st.PinnedTo, self)
		}
		selfPinned = true
	}
	if selfPinned {
		for _, s := range siblings {
			if _, ok := pinned[s]; !ok {
				pinned[s] = struct{}{}
				st.PinnedTo = append(st.PinnedTo, s)
			}
		}
	}
	fresh := c.clock.Now().UTC().Add(-integrity.DefaultFreshness).Format(timestampFormat)
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT r.peer_id, coalesce(nullif(p.site, ''), r.peer_id) FROM replicas r
		  JOIN peers p ON p.id = r.peer_id AND p.mode = 'full'
		 WHERE r.blob_hash = ? AND r.state = 'present'
		   AND (p.is_self = 1 OR coalesce(r.reported_at, '') >= ? OR coalesce(r.verified_at, '') >= ?)
		 ORDER BY r.peer_id`, blobHash, fresh, fresh)
	if err != nil {
		return PlacementStatus{}, fmt.Errorf("catalog: reading placement status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sites := map[string]struct{}{}
	for rows.Next() {
		var peer, site string
		if err := rows.Scan(&peer, &site); err != nil {
			return PlacementStatus{}, err
		}
		st.HeldAt = append(st.HeldAt, peer)
		sites[site] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return PlacementStatus{}, err
	}
	st.Sites = len(sites)
	if st.PinnedTo == nil {
		st.PinnedTo = []string{}
	}
	if st.HeldAt == nil {
		st.HeldAt = []string{}
	}
	if st.TakenFrom == nil {
		st.TakenFrom = []string{}
	}
	return st, nil
}

func (c *Catalog) collect(ctx context.Context, into *[]string, query string, args ...any) error {
	rows, err := c.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("catalog: reading placement status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		*into = append(*into, s)
	}
	return rows.Err()
}

// PlacementCounts is how far a two-site vault is from being held at both sites
// (ADR-0114, #662), for `system drift`.
type PlacementCounts struct {
	// Uploads is self-pinned blobs: what this node offers siblings.
	Uploads int64
	// UploadsBehind is uploads not known to be at both sites: not held here,
	// or no configured sibling is known, freshly, to hold them. A copy lost
	// at either site keeps it behind.
	UploadsBehind int64
	// Taken is placements taken from siblings.
	Taken int64
	// TakenBehind is taken placements whose bytes are not held here yet.
	TakenBehind int64
	// TakenUnservable is TakenBehind no usable peer holds WHOLE: no whole-blob
	// source. A swarm may still assemble it from pieces (ADR-0042), so this is
	// "no whole source", not "cannot converge".
	TakenUnservable int64
}

// PlacementDrift counts the placements not yet at both sites. siblings is this
// node's placement.siblings; another site's copy counts only while its claim is
// fresh (integrity.DefaultFreshness), the rule PlacementStatusOf uses.
func (c *Catalog) PlacementDrift(ctx context.Context, siblings []string) (PlacementCounts, error) {
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return PlacementCounts{}, err
	}
	sibJSON, err := json.Marshal(append([]string{}, siblings...))
	if err != nil {
		return PlacementCounts{}, err
	}
	fresh := c.clock.Now().UTC().Add(-integrity.DefaultFreshness).Format(timestampFormat)
	var pc PlacementCounts
	err = c.db.Reader().QueryRowContext(ctx, `
		WITH uploads AS (
			SELECT DISTINCT p.blob_hash AS hash FROM placement_pins p WHERE p.peer_id = ?
		), taken AS (
			SELECT DISTINCT pp.blob_hash AS hash FROM peer_placements pp
		), taken_behind AS (
			SELECT t.hash FROM taken t
			 WHERE NOT EXISTS (SELECT 1 FROM replicas r
			                    WHERE r.blob_hash = t.hash AND r.peer_id = ? AND r.state = 'present')
		)
		SELECT
		  (SELECT count(*) FROM uploads),
		  (SELECT count(*) FROM uploads u
		    WHERE NOT EXISTS (SELECT 1 FROM replicas mine
		                       WHERE mine.blob_hash = u.hash AND mine.peer_id = ? AND mine.state = 'present')
		       OR NOT EXISTS (
		      SELECT 1 FROM replicas r JOIN peers sp ON sp.id = r.peer_id AND sp.mode = 'full'
		       WHERE r.blob_hash = u.hash AND r.state = 'present'
		         AND r.peer_id IN (SELECT value FROM json_each(?))
		         AND (coalesce(r.reported_at, '') >= ? OR coalesce(r.verified_at, '') >= ?)
		         -- A sibling at this node's own site is the same failure domain.
		         AND coalesce(nullif(sp.site, ''), sp.id) <>
		             (SELECT coalesce(nullif(me.site, ''), me.id) FROM peers me WHERE me.is_self = 1))),
		  (SELECT count(*) FROM taken),
		  (SELECT count(*) FROM taken_behind),
		  (SELECT count(*) FROM taken_behind tb WHERE NOT EXISTS (
		      SELECT 1 FROM replicas r JOIN peers src ON src.id = r.peer_id
		       WHERE r.blob_hash = tb.hash AND r.peer_id <> ? AND r.state = 'present'
		         -- Usable as a source the way BlobSources ranks them: dialable and pinned.
		         AND coalesce(src.endpoint, '') <> '' AND length(coalesce(src.public_key, x'')) > 0))`,
		self, self, self, string(sibJSON), fresh, fresh, self,
	).Scan(&pc.Uploads, &pc.UploadsBehind, &pc.Taken, &pc.TakenBehind, &pc.TakenUnservable)
	if err != nil {
		return PlacementCounts{}, fmt.Errorf("catalog: counting placement drift: %w", err)
	}
	return pc, nil
}
