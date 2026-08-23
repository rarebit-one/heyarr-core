package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/capability"
)

// Worker capability advertisement, persisted (§6, §75, ADR-0037, M5-112).
//
// # Why this is stored rather than held in memory
//
// The same argument provider health makes, one plane over. Capabilities have
// existed since M1-05 as an in-memory field on a running worker, handed to
// jobs.Claim as an argument. That routes work and answers nothing: under
// invariant 4 the worker doing the encoding and the controller answering
// "which nodes can encode HEVC" may be different machines, so an advertisement
// that never left the worker's memory is not a fleet view, it is a local
// variable.
//
// # Three properties, and each one is a way this goes wrong
//
//   - It NARROWS. The stored set is made equal to the advertised set, removals
//     included. See AdvertiseCapabilities.
//   - It EXPIRES. Every reader filters on expires_at. See FleetCapabilities.
//   - It is EXACT. `capability = ?`, never LIKE — `ffmpeg` is a prefix of
//     `ffmpeg.encoder.hevc`.

// AdvertiseCapabilities makes the stored advertisement for one worker equal to
// the one given, and reports what that changed.
//
// # The whole set, in one transaction
//
// Rows this worker holds that the advertisement does not mention are DELETED.
// That is the narrowing, and it is expressed as "make the table equal this set"
// rather than as an explicit removal path, because an explicit removal path is
// something a future caller can forget to call. A device claimed by another
// process, or broken by a kernel update, changes nothing about the binary and
// changes nothing a caller would think to announce — the next beat simply
// advertises less, and less is what gets stored.
//
// One transaction so a reader never sees a half-applied advertisement: mid-way
// through, a worker would appear to have lost everything it holds, and a
// placement decision taken in that window would route around a healthy node.
//
// # Expired rows anywhere are swept here
//
// Not only this worker's. A worker identity is per PROCESS, so every restart
// leaves its predecessor's rows behind; without a sweep the table grows by one
// advertisement per restart forever. Doing it on the write path rather than on
// a beat of its own means it happens exactly as often as it needs to and needs
// no scheduling. It is housekeeping, NOT the expiry mechanism — the expiry
// mechanism is the filter in FleetCapabilities, because a stale row must not be
// honoured whether or not anybody has swept it yet.
//
// Idempotent (invariant 9): re-advertising the same set rewrites the same rows
// with a new expiry and reports no change, so a beat that runs twice is free.
func (c *Catalog) AdvertiseCapabilities(
	ctx context.Context, ad capability.Advertisement,
) (capability.Change, error) {
	if ad.WorkerID == "" {
		return capability.Change{}, fmt.Errorf("catalog: an advertising worker id is required")
	}
	if ad.TTL <= 0 {
		return capability.Change{}, fmt.Errorf(
			"catalog: worker %s advertised with no TTL; an advertisement that never expires "+
				"outlives the worker that made it", ad.WorkerID)
	}

	now := c.clock.Now().UTC()
	expires := now.Add(ad.TTL)

	var change capability.Change
	var ev events.Event
	var emitted bool

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		// Reset per attempt: InTx may retry, and a change set carried over from
		// a rolled-back attempt would report a narrowing that did not survive.
		change = capability.Change{}
		emitted = false

		before, err := heldByWorker(ctx, tx, ad.WorkerID)
		if err != nil {
			return err
		}

		want := map[string]capability.Held{}
		for _, h := range ad.Held {
			if h.Name == "" {
				return fmt.Errorf("catalog: worker %s advertised an unnamed capability", ad.WorkerID)
			}
			want[h.Name] = h
		}

		// THE NARROWING. Everything this worker holds that it no longer claims
		// goes, in one statement the database evaluates — not a diff computed
		// in Go and applied removal by removal, which is a loop somebody
		// eventually gets wrong in the direction of keeping too much.
		//
		// Deleting this statement leaves the upsert below, which can only ever
		// add and update. That is the grow-only advertisement ADR-0037 says
		// lies after the first driver update, and it is a sabotage the tests
		// are required to catch.
		keep := make([]any, 0, len(want)+1)
		// A sentinel that matches nothing, so an advertisement of NOTHING —
		// which is a legitimate state (ADR-0023) — produces valid SQL that
		// removes everything rather than an empty NOT IN ().
		keep = append(keep, "")
		for _, name := range sortedNames(want) {
			keep = append(keep, name)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
		args := append([]any{ad.WorkerID}, keep...)
		// #nosec G202 -- the only thing concatenated is a run of `?`
		// placeholders whose LENGTH comes from len(keep). Every capability
		// name travels as a bound argument; none of them reaches the SQL text.
		// The same shape as jobs.Claim's inClause, for the same reason: SQLite
		// has no array parameter.
		query := `DELETE FROM worker_capabilities
			 WHERE worker_id = ? AND capability NOT IN (` + placeholders + `)`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("catalog: narrowing worker %s's advertisement: %w", ad.WorkerID, err)
		}

		for _, name := range sortedNames(want) {
			h := want[name]
			provedAt := h.ProvedAt
			if provedAt.IsZero() {
				provedAt = now
			}
			source := h.Source
			if source == "" {
				source = capability.SourceProbe
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO worker_capabilities
					(worker_id, capability, peer_id, peer_name, source, proved_at, expires_at, detail)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (worker_id, capability) DO UPDATE SET
					peer_id    = excluded.peer_id,
					peer_name  = excluded.peer_name,
					source     = excluded.source,
					proved_at  = excluded.proved_at,
					expires_at = excluded.expires_at,
					detail     = excluded.detail`,
				ad.WorkerID, name, ad.PeerID, ad.PeerName, string(source),
				provedAt.UTC().Format(timestampFormat),
				expires.Format(timestampFormat), h.Detail); err != nil {
				return fmt.Errorf("catalog: advertising %s for worker %s: %w", name, ad.WorkerID, err)
			}
			if _, had := before[name]; !had {
				change.Gained = append(change.Gained, name)
			}
		}
		for _, name := range sortedNames(before) {
			if _, still := want[name]; !still {
				change.Lost = append(change.Lost, name)
			}
		}

		// Housekeeping, not the expiry mechanism. See the doc comment.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM worker_capabilities WHERE expires_at <= ?`,
			now.Format(timestampFormat)); err != nil {
			return fmt.Errorf("catalog: sweeping expired advertisements: %w", err)
		}

		if change.Empty() {
			return nil
		}
		// One event per CHANGE, with the transition in the payload — never one
		// per capability. A fleet of ten workers each holding a dozen
		// capabilities re-verifying every few minutes would otherwise write a
		// steady stream of events saying nothing happened, which is the failure
		// the package doc of internal/events describes.
		ev, err = c.events.EmitTx(ctx, tx, events.TypeWorkerCapabilitiesChanged,
			"worker", ad.WorkerID, map[string]any{
				"peer_id":   ad.PeerID,
				"peer_name": ad.PeerName,
				"gained":    change.Gained,
				"lost":      change.Lost,
			})
		if err != nil {
			return err
		}
		emitted = true
		return nil
	})
	if err != nil {
		return capability.Change{}, err
	}
	if emitted {
		c.events.Publish(ev)
	}
	return change, nil
}

// FleetCapabilities reads every LIVE advertisement, one entry per worker.
//
// # expires_at is the whole of the death detection
//
// A worker that dies cannot tidy up after itself: a power cut, an OOM kill and
// a severed partition are exactly the deaths that skip a shutdown hook, and
// they are the deaths that matter. So nothing here asks whether a worker is
// alive; it asks whether its claim is still in date. A row past its expiry is
// not returned, is not counted, and cannot be routed to — whether or not
// anything has got round to deleting it.
//
// The filter is `expires_at > now`, strictly. A row expiring exactly now has
// expired: the alternative honours a claim for one instant longer than the
// worker promised it, which is the wrong direction to round in.
//
// # only, when set, is an exact match
//
// `capability = ?`, never LIKE and never a prefix. A caller asking for `ffmpeg`
// wants nodes that have the binary, and a prefix match would answer with every
// node that can encode anything — the routing table's exactness is the reason
// the dotted vocabulary needed no schema change in the first place.
func (c *Catalog) FleetCapabilities(ctx context.Context, only string) ([]capability.Advertised, error) {
	now := c.clock.Now().UTC().Format(timestampFormat)

	query := `
		SELECT worker_id, capability, peer_id, peer_name, source, proved_at, expires_at, detail
		FROM worker_capabilities
		WHERE expires_at > ?`
	args := []any{now}
	if only != "" {
		// Exact. See the doc comment.
		query += ` AND capability = ?`
		args = append(args, only)
	}
	query += ` ORDER BY peer_name, worker_id, capability`

	rows, err := c.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading worker capabilities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []capability.Advertised
	byWorker := map[string]int{}
	for rows.Next() {
		var (
			workerID, name, peerID, peerName, source, detail string
			provedAt, expiresAt                              string
		)
		if err := rows.Scan(&workerID, &name, &peerID, &peerName, &source,
			&provedAt, &expiresAt, &detail); err != nil {
			return nil, err
		}
		idx, seen := byWorker[workerID]
		if !seen {
			out = append(out, capability.Advertised{
				WorkerID:  workerID,
				PeerID:    peerID,
				PeerName:  peerName,
				ExpiresAt: parseStamp(expiresAt).UTC(),
			})
			idx = len(out) - 1
			byWorker[workerID] = idx
		}
		out[idx].Held = append(out[idx].Held, capability.Held{
			Name:     name,
			Source:   capability.Source(source),
			Detail:   detail,
			ProvedAt: parseStamp(provedAt).UTC(),
		})
	}
	return out, rows.Err()
}

// heldByWorker reads what a worker currently advertises, expired or not.
//
// Deliberately unfiltered by expiry: this is the BEFORE picture for computing a
// change, and a row that has expired but not yet been swept was still being
// advertised as far as this worker's own history goes. Filtering it here would
// report a capability as "gained" every time the beat ran late.
func heldByWorker(ctx context.Context, tx *sql.Tx, workerID string) (map[string]capability.Held, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT capability, source, detail FROM worker_capabilities WHERE worker_id = ?`, workerID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading worker %s's advertisement: %w", workerID, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]capability.Held{}
	for rows.Next() {
		var name, source, detail string
		if err := rows.Scan(&name, &source, &detail); err != nil {
			return nil, err
		}
		out[name] = capability.Held{Name: name, Source: capability.Source(source), Detail: detail}
	}
	return out, rows.Err()
}

func sortedNames(m map[string]capability.Held) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
