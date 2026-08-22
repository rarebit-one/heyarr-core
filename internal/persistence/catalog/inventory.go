package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
)

// ReconcileInventory folds a peer's inventory report into `replicas` (§20,
// ADR-0029, M4-07).
//
// # peerID is the acting peer, and it is an argument for a reason
//
// It is passed in rather than read from report.PeerID. The caller derives it
// from the client certificate (ADR-0033), and this function never looks at the
// declaration in the body — so no future edit here can turn the body into the
// identity by accident. report.PeerID has already been compared against this
// value by the surface that authenticated the connection.
//
// This is also the first writer in this package that does NOT resolve
// SelfPeer. Every other one does, which is exactly why no `replicas` row has
// ever described a machine other than the one that wrote it.
//
// # What a report may do to a row
//
// It may add one, change one, and — the part that gets discovered late — take
// one AWAY, by moving it from `present` to `missing`. An inventory that could
// only add replicas converges on a table that never shrinks and always claims
// the library is safer than it is, and that table is what garbage collection
// consults before deleting what it believes is a surplus copy.
//
// Removals are `missing`, never deleted rows. A peer losing bytes has to be
// visible rather than silently absent.
//
// # Full and incremental
//
// Both produce the same `replicas` state for the same reality, and that is a
// property worth asserting rather than assuming. The difference is what each
// says about a blob it does not mention:
//
//   - a full report asserts absence, so a row this peer has that the report
//     omits becomes `missing`, and its freshness advances — the peer confirmed
//     that blob by excluding it;
//   - an incremental report asserts nothing, so an omitted row is untouched
//     and its freshness does NOT advance. It communicates a loss with an
//     explicit `missing` entry.
//
// # Idempotence (invariant 9)
//
// Two identical consecutive reports change nothing and emit no events. Every
// write below is conditional on the value actually differing, which is what
// makes a re-run — and it WILL be re-run — free rather than a second round of
// event noise.
func (c *Catalog) ReconcileInventory(ctx context.Context, peerID string, report inventory.Report) (inventory.Outcome, error) {
	if peerID == "" {
		return inventory.Outcome{}, fmt.Errorf("%w: the acting peer is required and comes from the "+
			"certificate, never from the report", inventory.ErrUnknownPeer)
	}
	if err := report.Validate(); err != nil {
		return inventory.Outcome{}, err
	}

	receivedAt := c.clock.Now().UTC()
	observed := report.ObservedAt.UTC().Format(timestampFormat)
	received := receivedAt.Format(timestampFormat)
	reportID := uuid.Must(uuid.NewV7()).String()

	outcome := inventory.Outcome{
		ReportID:   reportID,
		PeerID:     peerID,
		Mode:       report.Mode,
		Entries:    len(report.Entries),
		ObservedAt: report.ObservedAt.UTC(),
		ReceivedAt: receivedAt,
	}

	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		// Reset per attempt: InTx may retry, and counters carried over from a
		// rolled-back attempt would report work that did not survive.
		outcome.Added, outcome.Changed, outcome.Removed, outcome.Unknown = 0, 0, 0, 0
		pending = pending[:0]

		if err := peerExists(ctx, tx, peerID); err != nil {
			return err
		}

		held, err := heldReplicas(ctx, tx, peerID)
		if err != nil {
			return err
		}

		// One desired state per blob, assembled before anything is written, so
		// that "the peer says present" and "the peer's full report omitted it"
		// are the same kind of fact by the time they reach the writer. Two
		// separate write paths would be two places for the removal half to be
		// forgotten — and the removal half is the one that matters.
		desired := make(map[string]desiredReplica, len(report.Entries))
		for _, e := range report.Entries {
			desired[e.BlobHash] = desiredReplica{
				state:        string(e.State),
				bytesPresent: e.BytesPresent,
				verifiedAt:   e.VerifiedAt,
				// Reported by name: the peer said something about this blob,
				// so its freshness advances.
				confirmed: true,
			}
		}
		if report.Mode == inventory.ModeFull {
			for hash, absent := range absentAsMissing(held, desired) {
				desired[hash] = absent
			}
		}

		// Deterministic order. The rows are the same either way, but the
		// events are not: a map walk would emit them in a different order on
		// every run, and an acceptance assertion over the event log would be
		// asserting on iteration order (ADR-0017).
		hashes := make([]string, 0, len(desired))
		for hash := range desired {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)

		for _, hash := range hashes {
			want := desired[hash]
			current, known := held[hash]

			if !known {
				exists, err := blobExists(ctx, tx, hash)
				if err != nil {
					return err
				}
				if !exists {
					// A peer may legitimately hold bytes this controller has
					// never heard of — it was restored from a newer catalog,
					// or the blob row has been reclaimed here. There is no
					// blobs row to reference, so there can be no replicas row.
					// Counted and reported rather than swallowed or refused.
					outcome.Unknown++
					continue
				}
			}

			if known && current.matches(want) {
				// Nothing about the row changes. Freshness still advances if
				// this report confirmed the blob — that is the entire value of
				// a report that says "yes, still here": the state is the same
				// and the DATE is not.
				if want.confirmed && current.reportedAt != observed {
					if _, err := tx.ExecContext(ctx,
						`UPDATE replicas SET reported_at = ? WHERE blob_hash = ? AND peer_id = ?`,
						observed, hash, peerID); err != nil {
						return fmt.Errorf("catalog: stamping the freshness of %s on peer %s: %w", hash, peerID, err)
					}
				}
				continue
			}

			verified := want.verifiedStamp(current)
			reported := current.reportedAt
			if want.confirmed {
				reported = observed
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, reported_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
					state = excluded.state, bytes_present = excluded.bytes_present,
					verified_at = excluded.verified_at, reported_at = excluded.reported_at,
					updated_at = excluded.updated_at`,
				hash, peerID, want.state, want.bytesPresent,
				nullable(verified), nullable(reported), received); err != nil {
				return fmt.Errorf("catalog: recording peer %s's replica of %s: %w", peerID, hash, err)
			}

			switch {
			case !known:
				outcome.Added++
			case want.state == string(inventory.StateMissing):
				outcome.Removed++
			default:
				outcome.Changed++
			}

			ev, emitted, err := c.emitReplicaTransition(ctx, tx, hash, peerID, current, want, known)
			if err != nil {
				return err
			}
			if emitted {
				pending = append(pending, ev)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inventory_reports
				(id, peer_id, mode, observed_at, received_at, entries, added, changed, removed, unknown)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			reportID, peerID, string(report.Mode), observed, received,
			outcome.Entries, outcome.Added, outcome.Changed, outcome.Removed, outcome.Unknown); err != nil {
			return fmt.Errorf("catalog: recording peer %s's inventory report: %w", peerID, err)
		}

		// One event per report CYCLE, never one per blob. A first exchange
		// with a peer holding a hundred thousand blobs would otherwise put a
		// hundred thousand records in the log to say one thing — see the
		// package doc of internal/events.
		ev, err := c.events.EmitTx(ctx, tx, events.TypeSyncInventoryReported, "peer", peerID,
			map[string]any{
				"report_id":   reportID,
				"peer_id":     peerID,
				"mode":        string(report.Mode),
				"entries":     outcome.Entries,
				"added":       outcome.Added,
				"changed":     outcome.Changed,
				"removed":     outcome.Removed,
				"unknown":     outcome.Unknown,
				"observed_at": observed,
			})
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return inventory.Outcome{}, err
	}
	c.events.Publish(pending...)
	return outcome, nil
}

// desiredReplica is what a report says a row should become.
type desiredReplica struct {
	state        string
	bytesPresent int64
	verifiedAt   *time.Time
	// confirmed is whether this report actually SAYS something about the blob.
	//
	// It is what separates "the peer told us this blob is gone" from "the peer
	// did not mention it". Only the first advances freshness, and conflating
	// them would make an incremental report silently re-date rows nobody
	// looked at — which is how a stale `replicas` table starts looking fresh.
	confirmed bool
}

// verifiedStamp is the verification time to store.
//
// A report that carries none never CLEARS one. Collecting an inventory reads a
// directory rather than re-hashing a library, so "this report said nothing
// about verification" is the normal case, and treating it as "this blob has
// never been verified" would erase the record on every cycle.
func (d desiredReplica) verifiedStamp(current replicaRow) string {
	if d.verifiedAt == nil {
		return current.verifiedAt
	}
	return d.verifiedAt.UTC().Format(timestampFormat)
}

// replicaRow is one existing `replicas` row, as the reconciler needs it.
type replicaRow struct {
	state        string
	bytesPresent int64
	verifiedAt   string
	reportedAt   string
}

// matches reports whether the row already says what the report wants it to
// say. It is the idempotence check: two identical consecutive reports find
// every row matching, write nothing but freshness, and emit nothing.
func (r replicaRow) matches(want desiredReplica) bool {
	if r.state != want.state || r.bytesPresent != want.bytesPresent {
		return false
	}
	return want.verifiedAt == nil || want.verifiedAt.UTC().Format(timestampFormat) == r.verifiedAt
}

// absentAsMissing is the removal half, and the reason `replicas` can shrink.
//
// A full report names every blob its peer holds, so a row that peer has and
// the report does NOT name is a blob that peer no longer has. It becomes
// `missing` — not a deleted row, because a peer losing bytes must be visible
// rather than silently absent, and because a deleted row is indistinguishable
// from one that was never written.
//
// It is confirmed: absence from a full report is an assertion, not silence,
// so freshness advances. That is also what makes a full report and an
// incremental report of the same reality produce the same table — the
// incremental one says `missing` in an entry and the full one says it by
// omission, and neither may leave the row looking staler than the other.
//
// Rows already `missing` are skipped: re-writing them would emit a transition
// that did not happen and make every cycle after a loss look like a new loss.
func absentAsMissing(held map[string]replicaRow, named map[string]desiredReplica) map[string]desiredReplica {
	out := map[string]desiredReplica{}
	for hash, row := range held {
		if _, mentioned := named[hash]; mentioned {
			continue
		}
		if row.state == string(inventory.StateMissing) {
			continue
		}
		out[hash] = desiredReplica{
			state:        string(inventory.StateMissing),
			bytesPresent: 0,
			confirmed:    true,
		}
	}
	return out
}

// emitReplicaTransition emits replica.present / replica.corrupt /
// replica.missing for a row whose state actually moved.
//
// # Why a row this report CREATED does not emit
//
// internal/events draws the peer plane's line at work rather than at items: a
// first inventory exchange with a peer holding a hundred thousand blobs must
// not write a hundred thousand events. A row this report created is the
// controller LEARNING a fact, not a transition — it held no belief for that
// blob on that peer a moment ago, so there is nothing that changed. The cycle
// event carries the count, and the per-blob facts are in `replicas`, which is
// where the detail belongs.
//
// A row that already existed and moved is different in kind: the controller
// believed something and now believes something else. present → missing is a
// peer that lost bytes, and it is bounded by damage rather than by library
// size. That is the transition M4-12 and an operator both need to see, and it
// is why these three types get a non-self subject here for the first time.
func (c *Catalog) emitReplicaTransition(
	ctx context.Context, tx *sql.Tx, hash, peerID string,
	current replicaRow, want desiredReplica, known bool,
) (events.Event, bool, error) {
	if !known || current.state == want.state {
		return events.Event{}, false, nil
	}
	var eventType string
	switch inventory.State(want.state) {
	case inventory.StatePresent:
		eventType = events.TypeReplicaPresent
	case inventory.StateCorrupt:
		eventType = events.TypeReplicaCorrupt
	case inventory.StateMissing:
		eventType = events.TypeReplicaMissing
	default:
		// Unreachable: Validate refuses any other state before this runs.
		return events.Event{}, false, fmt.Errorf("catalog: peer %s reported state %q for %s, "+
			"which is not a state a peer may report", peerID, want.state, hash)
	}
	ev, err := c.events.EmitTx(ctx, tx, eventType, "blob", hash, map[string]any{
		"peer_id":       peerID,
		"previous":      current.state,
		"bytes_present": want.bytesPresent,
		// The subject is a blob and the peer is in the payload, exactly as the
		// self-peer emitters do it. `source` is what tells a subscriber this
		// transition came from a peer's own report of its disk rather than
		// from a local verification on this node.
		"source": "inventory_report",
	})
	if err != nil {
		return events.Event{}, false, err
	}
	return ev, true, nil
}

// heldReplicas reads every `replicas` row this peer already has.
//
// The whole set, rather than one lookup per entry: a full report asks about
// every blob anyway, and the absent-as-missing sweep needs rows the report did
// NOT mention — which no per-entry lookup can find by construction.
func heldReplicas(ctx context.Context, tx *sql.Tx, peerID string) (map[string]replicaRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT blob_hash, state, bytes_present, coalesce(verified_at, ''), coalesce(reported_at, '')
		 FROM replicas WHERE peer_id = ?`, peerID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading peer %s's replicas: %w", peerID, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]replicaRow{}
	for rows.Next() {
		var (
			hash string
			row  replicaRow
		)
		if err := rows.Scan(&hash, &row.state, &row.bytesPresent, &row.verifiedAt, &row.reportedAt); err != nil {
			return nil, fmt.Errorf("catalog: reading peer %s's replicas: %w", peerID, err)
		}
		out[hash] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading peer %s's replicas: %w", peerID, err)
	}
	return out, nil
}

// peerExists reports whether the acting peer has a catalog row.
//
// Checked explicitly rather than left to the foreign key, because the foreign
// key's error arrives per row and says "FOREIGN KEY constraint failed", which
// names neither the peer nor the disagreement between membership and the
// catalog that produced it.
func peerExists(ctx context.Context, tx *sql.Tx, peerID string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM peers WHERE id = ?`, peerID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", inventory.ErrUnknownPeer, peerID)
	case err != nil:
		return fmt.Errorf("catalog: looking up peer %s: %w", peerID, err)
	}
	return nil
}

// blobExists reports whether the catalog knows this blob at all.
func blobExists(ctx context.Context, tx *sql.Tx, hash string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM blobs WHERE hash = ?`, hash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: looking up blob %s: %w", hash, err)
	}
	return true, nil
}

// nullable renders an empty timestamp as SQL NULL.
//
// The distinction is load-bearing for reported_at: NULL means "no peer has
// ever confirmed this row", and an empty string would be a timestamp that
// parses as the zero time and reads as "confirmed in the year 1".
func nullable(stamp string) any {
	if stamp == "" {
		return nil
	}
	return stamp
}
