package catalog

import (
	"context"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Peer convergence (§19, §57, M4-08).
//
// # This file is the EDGE, and the diff is in the domain
//
// The same division reconcile.go makes: everything here is a query, and the
// decision is a pure function in internal/domain/replication. What lives here
// is the mapping FROM rows TO the two sets that function takes — the canonical
// blob set, and what each peer is known to hold — and nothing here decides
// anything.
//
// # requiredPeers is reused deliberately
//
// The Full Peer set is already computed, in reconcile.go, for placement
// evaluation. It is §19's whole placement policy and it is correct: every peer
// in `full` mode holds everything. What changes in this milestone is not the
// query but its result — it returns two rows for the first time, and every
// branch downstream of it that was written against a hypothetical second peer
// finally runs against a real one.
//
// §34's placement policies are a later milestone. There is no policy table
// here and there must not be one until there is.

// PeerConvergence is what one reconciliation cycle's diff concluded, before
// any work is enqueued.
//
// It is a value rather than a side effect so that "what does the fabric look
// like" can be asked without changing anything — which is what makes the
// convergence property testable across cycles rather than inferred from a log.
type PeerConvergence struct {
	// Scope is the peer this cycle was restricted to, or empty for the whole
	// fabric.
	Scope string
	// Peers are the Full Peers this cycle considered, in id order.
	Peers []string
	// Desired is the size of the canonical blob set — §19's
	// desired_blob_set(peer) for a Full Peer.
	Desired int
	// Gaps is every (blob, peer) the desired set requires and no peer
	// inventory accounts for, in deterministic order.
	Gaps []replication.Gap
}

// PlanPeerConvergence diffs the desired blob set against what the peers hold.
//
// Read-only, by design and not by accident. A cycle that decided nothing must
// leave nothing behind, and a planner that wrote as it went could not be run
// twice in a test to prove the second run found less.
//
// scope restricts the cycle to one peer; empty means every Full Peer. A scope
// naming a peer that is not a Full Peer yields no peers and no gaps rather
// than an error: a peer removed or demoted between enqueue and run is an
// ordinary race, and failing the job would retry it five times to no purpose.
func (c *Catalog) PlanPeerConvergence(ctx context.Context, scope string) (PeerConvergence, error) {
	required, err := c.requiredPeers(ctx)
	if err != nil {
		return PeerConvergence{}, err
	}
	if scope != "" {
		kept := required[:0:0]
		for _, id := range required {
			if id == scope {
				kept = append(kept, id)
			}
		}
		required = kept
	}

	plan := PeerConvergence{Scope: scope, Peers: required}
	if len(required) == 0 {
		return plan, nil
	}

	canonical, err := c.canonicalBlobs(ctx)
	if err != nil {
		return PeerConvergence{}, err
	}
	plan.Desired = len(canonical)

	held, err := c.presentReplicas(ctx)
	if err != nil {
		return PeerConvergence{}, err
	}

	peers := make([]replication.Peer, 0, len(required))
	for _, id := range required {
		// Every id here came out of a query filtered on mode = 'full', so the
		// mode is not re-read: it is a property of the query, and carrying it
		// back out of the row would invite the two to disagree.
		peers = append(peers, replication.Peer{ID: id, Mode: replication.ModeFull})
	}
	plan.Gaps = replication.Diff(peers, canonical, held)
	return plan, nil
}

// canonicalBlobs is the canonical blob set: every blob the catalog still
// accounts for through a live asset.
//
// # Why assets and not the blobs table
//
// `blobs` is every blob this node has ever hashed, including ones nothing
// references any more — which is exactly what garbage collection is about to
// reclaim (ADR-0018). Replicating those would mean shipping bytes to a second
// site so that a sweep can delete them at both ends, and worse, it would race
// the sweep: a blob could be transferred and collected in either order.
//
// So the canonical set is what the CATALOG still claims, which is what a
// second site is for.
//
// # Linked assets are absent by construction (ADR-0020)
//
// There is no source_class filter in this query and there must not be one. A
// linked asset has no blob — the schema's own CHECK enforces blob_hash IS NULL
// for it — so `blob_hash IS NOT NULL` excludes it as a consequence of the data
// model rather than as a rule someone remembered to write. Replication, as the
// ADR promises, needs no special case: there is simply nothing to operate on.
//
// missing_since is respected for the same reason: an asset whose bytes have
// gone is not something to go and replicate.
func (c *Catalog) canonicalBlobs(ctx context.Context) ([]string, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT DISTINCT blob_hash FROM assets
		 WHERE blob_hash IS NOT NULL AND missing_since IS NULL
		 ORDER BY blob_hash`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the canonical blob set: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// presentReplicas is what every peer is known to hold.
//
// `present` only, the same rule replicasOf applies and for the same reason: a
// pending replica is bytes that have not arrived, and a corrupt one is bytes
// that arrived wrong and are quarantined (ADR-0018). Counting either as held
// would make the fabric report convergence it has not reached — and a `missing`
// row, which is a peer telling us it lost the bytes, is the very case
// reconciliation exists to notice.
func (c *Catalog) presentReplicas(ctx context.Context) (replication.Holdings, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT peer_id, blob_hash FROM replicas WHERE state = 'present'`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the present replicas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	held := replication.Holdings{}
	for rows.Next() {
		var peerID, hash string
		if err := rows.Scan(&peerID, &hash); err != nil {
			return nil, err
		}
		if held[peerID] == nil {
			held[peerID] = map[string]struct{}{}
		}
		held[peerID][hash] = struct{}{}
	}
	return held, rows.Err()
}

// PeerReconcileSummary is what one cycle did, as the cycle event carries it.
type PeerReconcileSummary struct {
	// Scope is the peer the cycle was restricted to, or empty for the fabric.
	Scope string
	// Peers is how many Full Peers the cycle considered.
	Peers int
	// Desired is the size of the canonical blob set.
	Desired int
	// UnderReplicated is how many (blob, peer) pairs the diff found.
	UnderReplicated int
	// InFlight is how many of those already had a live replicate_blob job, so
	// this cycle correctly did nothing about them.
	InFlight int
	// Enqueued is how many replicate_blob jobs this cycle created.
	Enqueued int
	// Deferred is how many gaps the per-cycle bound left for a later cycle.
	// Reported rather than dropped: a cycle that hit its bound has NOT
	// converged, and a summary that omitted this would look exactly like one
	// that had.
	Deferred int
}

// RecordPeerReconciled emits sync.reconciled — one event per cycle.
//
// One event, carrying counts, and never one per enqueued job: job.enqueued
// already reports that, per job, and repeating it here would say the same thing
// twice in two vocabularies. See the "No per-blob events during replication"
// note in internal/events.
//
// A cycle that decided to do nothing still emits. "We looked and everything is
// where it should be" is the outcome an operator most needs and the only one
// that leaves no job rows behind to prove it happened — and it is also the
// observation that distinguishes a converged fabric from a reconciler that has
// silently stopped running.
func (c *Catalog) RecordPeerReconciled(ctx context.Context, s PeerReconcileSummary) error {
	// The subject is this node: the cycle is something the controller did,
	// and the peers it decided about are counted in the payload. A cycle
	// scoped to one peer names it in `scope` rather than in the subject, so
	// that every sync.reconciled event is the same shape and a subscriber can
	// read the series without branching.
	self, err := c.SelfPeer(ctx)
	if err != nil {
		return err
	}
	scope := s.Scope
	if scope == "" {
		scope = "fabric"
	}
	if _, err := c.events.Emit(ctx, events.TypeSyncReconciled, "peer", self, map[string]any{
		"scope":            scope,
		"peers":            s.Peers,
		"desired":          s.Desired,
		"under_replicated": s.UnderReplicated,
		"in_flight":        s.InFlight,
		"enqueued":         s.Enqueued,
		"deferred":         s.Deferred,
	}); err != nil {
		return fmt.Errorf("catalog: recording a peer reconciliation cycle: %w", err)
	}
	return nil
}
