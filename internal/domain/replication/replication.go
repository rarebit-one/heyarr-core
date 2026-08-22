// Package replication is peer convergence: what a peer should hold, what it
// does hold, and the difference between the two (§19, §57, §75, M4-08).
//
// # A pure diff, and why that is the whole design
//
// §19 states the policy in one line — a Full Peer's desired blob set is the
// complete canonical blob set — and §57 names the reconciliation domain: the
// global desired blob set against a peer's inventory yields the missing blobs,
// which yield replication. Nothing in that chain is stateful. Reconciliation
// holds no cursor, remembers no previous cycle and has no notion of progress:
// it looks at what is desired, looks at what is held, and reports what is
// absent.
//
// That is what makes convergence provable rather than hoped for. A reconciler
// that carried state could drift from reality and go on emitting work for a
// transfer that already finished; one that recomputes the difference every
// cycle cannot, because the difference shrinks the moment the inventory says
// so. Run it twice against an unchanged fabric and it produces the same
// answer; run it after a transfer lands and it produces strictly less.
//
// The distinction that matters, and that a single run cannot show: a
// reconciler which re-emits the same transfer every cycle is not converging,
// it is looping. Both look identical in one pass. It is only across cycles
// that the difference appears, which is why the diff below is a function of
// two sets and of nothing else, and why the work it emits is keyed on
// blob_hash + destination peer so that the job queue's unique index over live
// jobs (ADR-0008, invariant 9) makes the second cycle harmless by construction
// rather than by care.
//
// # What is NOT here
//
// §34's placement policies — replica counts, distinct failure domains — are a
// later milestone. Until then §19's full-replica default is the entire
// placement policy, and saying so plainly is more honest than a policy table
// nothing writes. The peer set this operates over is every peer in `full`
// mode, and that is the only rule.
//
// This package decides WHAT should move. It never moves it: emitting
// replicate_blob work and stopping there is the boundary, and the transfer
// itself is M4-09.
package replication

import "sort"

// ModeFull is the peer mode §19 gives its default to: a Full Peer holds
// everything.
//
// Spelled here rather than imported from persistence because this package is
// domain and must not know how a peer is stored (invariant 2). The value is
// the one the `peers.mode` CHECK constrains.
const ModeFull = "full"

// Peer is what a placement decision knows about a peer: who it is and what
// kind of peer it is. Deliberately not its endpoint, its health or its
// capacity — none of those change what it SHOULD hold, and a diff that
// consulted them would silently stop converging whenever a peer was briefly
// unreachable.
type Peer struct {
	ID   string
	Mode string
}

// Gap is one blob that one peer should hold and does not.
//
// It is the unit of work reconciliation emits, and it is a pair rather than a
// blob because the same blob may be missing from several peers and each
// absence is its own transfer.
type Gap struct {
	BlobHash string
	PeerID   string
}

// DedupeKey is the queue's idempotency key for closing this gap.
//
// blob_hash + destination peer, which is the identity of the work: there is
// exactly one useful answer to "get these bytes onto that machine", and two
// jobs saying it are one job said twice. The queue's partial-unique index over
// live jobs (ADR-0008) turns that into an enforced property rather than a
// convention, so a second reconciliation cycle over an unchanged fabric
// creates nothing.
//
// The destination is in the key and the SOURCE is not, deliberately. Which
// peer the bytes come from is a routing decision the transfer makes (ADR-0030
// — replication is a destination pull); putting it in the identity would make
// two jobs that want the same outcome look different because they picked
// different sources.
func (g Gap) DedupeKey() string { return "replicate:" + g.BlobHash + ":" + g.PeerID }

// DesiredBlobSet is §19's desired_blob_set(peer).
//
// # Linked assets are outside the set BY CONSTRUCTION
//
// ADR-0020's load-bearing choice is that a linked asset has no Blob at all —
// not a mutable one, not a special one, none. So it cannot appear in the
// canonical blob set, cannot be diffed, and cannot produce replication work.
// There is no filter here excluding it and there must never be one: the moment
// this package needed a `if sourceClass == "linked"` branch, the ADR's promise
// that replication needs no special cases would have quietly stopped being
// true. The caller supplies blobs; a linked asset simply has none to supply.
//
// # Modes other than full
//
// Anything that is not a Full Peer gets an empty set, which is the honest
// answer while §34 is unbuilt: Heyarr has no policy that says what a partial,
// cache or archive peer should hold, and inventing one here — "everything, but
// smaller" — would be a placement policy smuggled in as a default. An empty
// desired set means such a peer is never a replication destination, which is
// the conservative failure: no bytes move to a peer nobody has decided about.
func DesiredBlobSet(p Peer, canonical []string) []string {
	if p.Mode != ModeFull {
		return nil
	}
	out := make([]string, len(canonical))
	copy(out, canonical)
	return out
}

// Holdings is what each peer is known to hold: peer id → set of blob hashes.
type Holdings map[string]map[string]struct{}

// Diff is the reconciliation itself: every (blob, peer) the desired set
// requires and the holdings do not have.
//
// Ordered by blob and then by peer, so the answer is a value rather than a map
// walk (ADR-0017). Determinism here is not tidiness: the bound below takes a
// PREFIX of this slice, and a prefix of a randomly ordered list would replicate
// a different arbitrary subset every cycle instead of finishing what it
// started.
func Diff(peers []Peer, canonical []string, held Holdings) []Gap {
	var gaps []Gap
	for _, p := range peers {
		for _, hash := range DesiredBlobSet(p, canonical) {
			if _, ok := held[p.ID][hash]; ok {
				continue
			}
			gaps = append(gaps, Gap{BlobHash: hash, PeerID: p.ID})
		}
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].BlobHash != gaps[j].BlobHash {
			return gaps[i].BlobHash < gaps[j].BlobHash
		}
		return gaps[i].PeerID < gaps[j].PeerID
	})
	return gaps
}

// Bound takes at most limit gaps and reports how many were left.
//
// A first sync of a large library must not enqueue the whole catalogue in one
// cycle: a hundred thousand pending jobs is a queue no operator can see past,
// and it starves every other job type behind work that will take days. The
// bound makes the first cycle enqueue a slice of it and the next cycle take
// the next slice, which converges at the same rate — the transfers are the
// bottleneck, not the rows.
//
// The remainder is RETURNED rather than dropped. A caller that discarded it
// silently would produce a system that looks converged (nothing left to do
// this cycle) while being nowhere near it, and the count is what a log line
// and the cycle event carry so the difference is visible.
//
// limit <= 0 means no bound.
func Bound(gaps []Gap, limit int) (taken []Gap, deferred int) {
	if limit <= 0 || len(gaps) <= limit {
		return gaps, 0
	}
	return gaps[:limit], len(gaps) - limit
}
