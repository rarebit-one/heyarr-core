package integrity

import (
	"context"
	"errors"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The placement precondition on garbage collection (ADR-0018, §19, §53, §56,
// M4-12).
//
// # The rule, stated so it cannot be softened by accident
//
// Garbage collection refuses to unlink a blob's bytes unless it can
// AFFIRMATIVELY ESTABLISH that another peer holds verified bytes, recently
// enough to believe.
//
// The distinction between that and "unless it knows otherwise" is the whole
// deliverable. The second shape passes every happy-path test, and then deletes
// the last copy the first time a peer is down, a report is late, a row is
// wrong, or a database is empty — which is to say, exactly when it matters. So
// every path through [Collector.durability] that does not END at a positive,
// checked answer produces a refusal, and each refusal names itself.
//
// # Three ways to fail to establish it, and each one is watched
//
//   - the peer is UNREACHABLE, so nothing can be established at all;
//   - its inventory is STALE past the freshness bound (00023's reported_at),
//     so what is known is a fact about the past;
//   - it CLAIMS a replica it does not have, and the claim was checked.
//
// The third looks excessive until it is named properly: a `replicas` row is
// the controller's BELIEF, and the premise of this entire milestone is that
// beliefs and bytes diverge. Before the last local copy goes, the remote copy
// is verified to exist. A row that turns out to be a lie is corrected to
// `missing` on the way past, so the next sweep does not have to discover it
// again.
//
// # The one deployment this does not apply to
//
// A deployment with exactly one peer has no "elsewhere" for a placement policy
// to be satisfied at (ADR-0010: the peer model exists from Milestone 1 with
// exactly one peer). Refusing there would not protect anything; it would mean
// no single-node Heyarr could ever reclaim a byte, which is a different way of
// losing the library. That case is admitted, it is recorded as evidence with a
// `sole_peer` basis rather than passed over in silence, and the moment a second
// peer exists it stops applying — see [Collector.durability].

// Reason is why a sweep spared a blob — the refusals the durability
// precondition produces, as an operator reads them.
//
// They are strings rather than an enum because they are written into a
// Collection that an operator reads and a script parses, and a refusal nobody
// can read is an outage nobody can diagnose.
type Reason string

// The reasons a sweep spares a blob.
const (
	// ReasonNoOtherPeer is a multi-peer deployment where no other peer claims
	// this blob at all. Nothing to verify means nothing established.
	ReasonNoOtherPeer Reason = "no_other_peer"
	// ReasonReplicaNotPresent is another peer having a row that already says
	// pending, corrupt or missing. It is a claim not to hold the bytes.
	ReasonReplicaNotPresent Reason = "replica_not_present"
	// ReasonStaleInventory is a claim last confirmed longer ago than the
	// freshness bound — or never confirmed at all, which reported_at records
	// as NULL precisely so the two cannot be conflated.
	ReasonStaleInventory Reason = "stale_inventory"
	// ReasonPeerUnreachable is a peer that answered nothing.
	ReasonPeerUnreachable Reason = "peer_unreachable"
	// ReasonUnverifiable is a peer that failed to answer for a reason that is
	// neither "nothing is there" nor "I do not have it" — a refused
	// handshake, a malformed endpoint, a certificate this node will not
	// accept. It is kept distinct from unreachability because it is a
	// different thing to go and fix, and because collapsing it into either
	// neighbour would let an implementation bug read as a peer's answer.
	ReasonUnverifiable Reason = "unverifiable"
	// ReasonRemoteLacksBlob is the lying row: the catalog says present, the
	// peer says 404.
	ReasonRemoteLacksBlob Reason = "remote_lacks_blob"
	// ReasonDurabilityUnwired is a multi-peer deployment whose collector was
	// built without a way to reach peers. It is a refusal rather than a pass
	// because a collector that cannot check is a collector that must not
	// delete — a missing dependency must never read as a satisfied condition.
	ReasonDurabilityUnwired Reason = "durability_unwired"
	// ReasonControllerUnreachable is §53's "delete replicas: No". A peer cut
	// off from the control plane is working from a catalog nothing is
	// correcting, and that is precisely the peer ADR-0018 warns about.
	ReasonControllerUnreachable Reason = "controller_unreachable"
	// ReasonCatalogVacuous is the non-vacuity guard on the untracked sweep: a
	// catalog that considered nothing, or implausibly little, against a store
	// that is full of bytes.
	ReasonCatalogVacuous Reason = "catalog_vacuous"
)

// Bases on which a blob's durability was established, recorded into
// durability_evidence.
const (
	// BasisVerifiedRemote is another peer asked and answering that it holds
	// the bytes.
	BasisVerifiedRemote = "verified_remote"
	// BasisSolePeer is a deployment with no other peer — see the package note
	// above.
	BasisSolePeer = "sole_peer"
)

// What a [Durability] implementation may report back.
var (
	// ErrPeerUnreachable means nothing answered. It is silence, not a status:
	// a peer that answered anything at all is reachable, exactly as
	// internal/peer/health defines it.
	ErrPeerUnreachable = errors.New("integrity: that peer answered nothing")
	// ErrPeerLacksBlob means the peer answered, and answered that it does not
	// hold these bytes. This is the lying row, and it is a different fact from
	// unreachability: one says the claim is unverifiable, the other says the
	// claim is false.
	ErrPeerLacksBlob = errors.New("integrity: that peer does not hold these bytes")
	// ErrControllerUnreachable means the control plane could not be reached.
	ErrControllerUnreachable = errors.New("integrity: the controller could not be reached")
)

// DefaultFreshness is how recently another peer must have confirmed a replica
// for that claim to be usable as a reason to delete the last local copy.
//
// One hour. The reconciliation beat is five minutes, so a healthy peer
// confirms twelve times inside it and a peer that has gone quiet for an hour
// has missed twelve chances — that is a peer with a problem, not a peer having
// a slow moment. It is also four times the reachability window
// (health.DefaultWindow, fifteen minutes), so a peer is called unreachable
// long before its inventory is called stale: the two refusals stay distinct
// facts rather than one arriving dressed as the other.
//
// Against the grace window it is tiny — an hour against a week — and that
// asymmetry is right. The window measures how long a mistake stays reversible;
// this measures how old a belief may be before it stops being evidence.
const DefaultFreshness = time.Hour

// Peer is another node, as garbage collection needs it.
//
// It is declared here rather than imported from internal/peer because the
// Storage Fabric may not depend on the rest of Heyarr (§18, ADR-0007). It
// carries only what a durability check acts on: who to ask, where, and what
// this node last knew about whether anything is there.
type Peer struct {
	PeerID string
	Name   string
	// Endpoint is where the peer surface listens. Empty means there is no way
	// to ask, which is a refusal and never a pass.
	Endpoint string
	// PublicKey pins the connection. A candidate with no key is one membership
	// cannot vouch for (ADR-0012).
	PublicKey []byte
	// Health is the stored reachability answer (internal/peer/health). It is
	// consulted as a cheap first filter and is NEVER the final word — the
	// stored column is a belief like any other, so a peer it calls reachable
	// is still asked.
	Health     string
	LastSeenAt time.Time
}

// Reachable reports whether the stored health column says anything is there.
//
// Unknown is not reachable. A peer nothing has ever heard from has not been
// shown to be up, and treating the default as a pass is the failure the
// `unknown` state exists to make impossible.
func (p Peer) Reachable() bool { return p.Health == "reachable" }

// Replica is what the catalog BELIEVES another peer holds — one `replicas`
// row, joined to the peer it names.
//
// The name of this type is the warning. Nothing in it is evidence; all of it
// is a reason to go and check.
type Replica struct {
	Peer  Peer
	State string
	// BytesPresent is what the peer last reported holding.
	BytesPresent int64
	// VerifiedAt is when those bytes were last re-hashed. Zero means never.
	VerifiedAt time.Time
	// ReportedAt is when that peer last CONFIRMED this row in an inventory
	// report (00023). Zero means no peer ever has, which is a fact and not a
	// missing value — see the migration.
	ReportedAt time.Time
}

// Present reports whether the row claims the peer holds the bytes.
func (r Replica) Present() bool { return r.State == "present" }

// Fresh reports whether the claim was confirmed recently enough to believe.
//
// A never-confirmed row is not fresh. That is the whole reason reported_at was
// left un-backfilled by 00023: inventing a confirmation time from verified_at
// would have manufactured the one fact this test depends on.
func (r Replica) Fresh(now time.Time, within time.Duration) bool {
	if r.ReportedAt.IsZero() {
		return false
	}
	return now.Sub(r.ReportedAt) <= within
}

// Evidence is why one blob was believed durable elsewhere, written down BEFORE
// the delete that relied on it.
//
// Ordering is the point. `replicas.blob_hash` is ON DELETE CASCADE, so the
// transaction that removes the `blobs` row also removes every record of who
// else held it; evidence written afterwards would have nothing left to read.
// See migration 00028.
type Evidence struct {
	BlobHash   hashing.Hash
	Size       int64
	Basis      string
	PeerID     string
	PeerName   string
	Endpoint   string
	ReportedAt time.Time
	VerifiedAt time.Time
	Detail     string
	RecordedAt time.Time
}

// Sparing is a blob a sweep declined to reclaim, and why.
//
// It is reported rather than merely logged because `heyarr gc` telling an
// operator that it removed nothing, without saying what stopped it, is an
// outage nobody can diagnose. Every field here answers a question the operator
// is about to ask.
type Sparing struct {
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
	Reason Reason `json:"reason"`
	// Detail is the sentence a human reads.
	Detail string `json:"detail"`
	// PeerID and PeerName name the peer the refusal is ABOUT, when one is.
	PeerID   string `json:"peer_id,omitempty"`
	PeerName string `json:"peer_name,omitempty"`
}

// Durability is the remote half of the precondition: the ability to ask
// another machine a question and get an answer that is not a belief.
//
// It is an interface here and implemented in internal/peer/durability, for the
// reason every other port in this package is: the Storage Fabric must stay
// extractable and may not import the peer fabric or the API (§18, ADR-0007).
//
// A nil Durability in a multi-peer deployment refuses. It does not pass.
type Durability interface {
	// Controller reports whether the control plane can be reached from here.
	//
	// §53's table says "delete replicas: No" during a controller outage, and
	// until this method existed garbage collection had no way to tell — it
	// runs on local SQLite and a local CAS and consults nothing. A peer
	// running `gc --apply` while cut off is exactly the scenario ADR-0018
	// warns about, and it was reachable by anybody with a shell.
	//
	// It returns ErrControllerUnreachable when it cannot be reached.
	Controller(ctx context.Context) error

	// Holds asks a peer whether it serves these bytes RIGHT NOW.
	//
	// nil means it does. ErrPeerLacksBlob means it answered and said no.
	// ErrPeerUnreachable means nothing answered at all. Those three are
	// different actions and an implementation that collapses any two of them
	// has removed a refusal.
	Holds(ctx context.Context, p Peer, h hashing.Hash) error
}
