package replication

// The peer-convergence job identities (§75, ADR-0002, ADR-0008).
//
// Declared in the domain rather than in the worker for the reason
// acquisition's job constants are: the controller enqueues these and a worker
// runs them, and neither should have to import the other. Roles communicate
// only through the job table and HTTP (invariant 4), even inside `heyarr all`.

// ReconcilePeerJobType is §75's reconcile_peer: diff the desired blob set
// against what the peers report holding, and emit the transfers that close the
// difference.
//
// A separate job type from reconcile_desired, not a mode of it. The two ask
// different questions of different tables on different terms: reconcile_desired
// asks whether a WANT is satisfied by the bytes this node holds, and this asks
// whether the BYTES are everywhere they should be. Fusing them would tie a
// question about the catalog to a question about the fabric, and a fabric with
// no second peer would spend half of every run being a no-op.
const ReconcilePeerJobType = "reconcile_peer"

// ReconcilePeerDedupeKey is the queue's idempotency key for a fabric-wide
// cycle.
//
// ONE key for the whole cycle rather than one per peer, for the reason the
// desired-item sweep gives: two concurrent cycles would each read the fabric
// while the other enqueued against it, and the loser would spend the pass
// deciding against a picture that had already moved. A cycle already queued or
// running is the same cycle.
const ReconcilePeerDedupeKey = "reconcile:peer"

// ScopedReconcilePeerDedupeKey is the key for a cycle scoped to one peer.
//
// Scoped cycles dedupe per peer so that enrolling three peers at once queues
// three quick cycles rather than collapsing into one — the same shape as a
// scoped reconcile_desired.
func ScopedReconcilePeerDedupeKey(peerID string) string {
	if peerID == "" {
		return ReconcilePeerDedupeKey
	}
	return ReconcilePeerDedupeKey + ":" + peerID
}

// ReconcilePeerPayload is what a reconcile_peer job carries.
type ReconcilePeerPayload struct {
	// PeerID scopes the cycle to one peer. Empty means every Full Peer, which
	// is the scheduled case.
	//
	// The scoped form exists because a peer that was just enrolled, or that
	// has just reported an inventory, should not wait for the next fabric-wide
	// cycle to find out what it is missing.
	PeerID string `json:"peer_id,omitempty"`
}

// ReplicateBlobJobType is §75's replicate_blob: get one blob onto one peer.
//
// Reconciliation emits it and STOPS. The handler that moves bytes is M4-09,
// and until it exists these jobs stay pending and visible — which is the
// correct intermediate state rather than a gap: an operator can see exactly
// what the fabric has decided must move, before anything moves.
const ReplicateBlobJobType = "replicate_blob"

// ReplicateBlobPayload is what a replicate_blob job carries.
//
// The destination and the blob, and nothing else. No source peer: replication
// is a destination pull (ADR-0030) and the destination chooses where to pull
// from at the time it pulls, which may not be the peer that looked best when
// the job was written. A payload naming a source would be a payload that goes
// stale, and a job that ran against a five-minute-old routing decision is a
// job operating on a guess.
type ReplicateBlobPayload struct {
	BlobHash string `json:"blob_hash"`
	// DestinationPeerID is the peer that must end up holding the bytes.
	DestinationPeerID string `json:"destination_peer_id"`
}
