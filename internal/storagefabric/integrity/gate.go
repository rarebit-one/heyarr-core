package integrity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// The non-vacuity guard's two numbers (finding 2, M4-12).
//
// The hazard is specific: an empty or wrong database pointed at a populated
// CAS. Every file in the store then has no `blobs` row, every one of them looks
// exactly like the orphan an ingest fault leaves behind (M1-10), and a single
// `gc --apply` unlinks the library. Nothing in the four existing preconditions
// touches that path, because all four are about the catalog row a blob has and
// this path is about the row it does not.
//
// The guard cannot simply be "refuse when the catalog is empty", because a
// genuinely empty catalog with a couple of orphans is the ingest-fault case
// this sweep was built for and refusing there would break it. So it is a floor
// and a ratio together, and the two catch different halves:
//
//   - the RATIO says the store must corroborate the catalog. A healthy library
//     has a handful of orphans against thousands of tracked blobs; a wrong
//     database has untracked bytes as far as it can see. More than half the
//     store being untracked is not a sweep, it is a mismatch.
//   - the FLOOR keeps the ratio from firing on the small cases where it is
//     meaningless — one orphan in a store of one is 100% untracked and is also
//     exactly what a rolled-back ingest leaves. Below the floor the ratio has
//     no information in it.
//
// Eight is deliberately low. Above it, the ratio is doing the deciding, and the
// cost of being wrong in this direction is a sweep that reports why it declined
// and asks to be run again — against a cost, in the other direction, of the
// library.
const (
	// VacuityFloor is how many untracked files may be unlinked before the
	// catalog has to corroborate that this is its own store.
	VacuityFloor = 8
)

// SweepRefusal is a condition that stopped the whole sweep rather than one
// blob.
//
// Separate from [Sparing] because it is a different question: Sparing answers
// "why is this blob still here", SweepRefusal answers "why did this run decline
// to do its job". Collapsing them would put the same sentence on ten thousand
// rows and still leave the run-level answer to be inferred.
type SweepRefusal struct {
	Reason Reason `json:"reason"`
	Detail string `json:"detail"`
}

// gate is what one sweep established about its right to delete anything at
// all, computed once rather than per blob.
//
// Its zero value refuses nothing and permits nothing on its own — every field
// is consulted explicitly in establish, so a gate that failed to be built
// cannot read as a gate that passed.
type gate struct {
	// sole is a deployment with exactly one peer: no elsewhere for a placement
	// policy to be satisfied at. See durability.go.
	sole bool
	// refusals stop this sweep from unlinking anything. Non-empty means every
	// eligible blob is spared with the first of them.
	refusals []SweepRefusal
}

// gate establishes the sweep-wide preconditions: is there anywhere else at
// all, can this node reach a peer to ask, and can it reach the controller.
//
// # §53 is the one that was unreachable before this existed
//
// The spec's degraded-operation table says "delete replicas: No" during a
// controller outage. `heyarr gc` and the `gc_blobs` job operate purely on local
// SQLite and the local CAS: neither consulted the controller, neither knew what
// degraded meant, and a peer running `gc --apply` while cut off — which is
// precisely the scenario ADR-0018 warns about — was reachable by anybody with a
// shell. The check below is what makes that sentence in the spec enforceable
// rather than aspirational.
func (c *Collector) gate(ctx context.Context) (gate, error) {
	peers, err := c.opts.Catalog.Peers(ctx)
	if err != nil {
		return gate{}, err
	}
	g := gate{sole: len(peers) == 0}

	if len(peers) > 0 && c.opts.Durability == nil {
		// A missing dependency must never read as a satisfied condition. This
		// collector has another peer to be durable ON and no way to ask it
		// anything, so it establishes nothing and unlinks nothing.
		g.refusals = append(g.refusals, SweepRefusal{
			Reason: ReasonDurabilityUnwired,
			Detail: fmt.Sprintf("this deployment has %d other peer(s) and this collector was built "+
				"with no way to reach them, so it cannot establish that any blob exists "+
				"elsewhere (ADR-0018)", len(peers)),
		})
		return g, nil
	}
	if c.opts.Durability == nil {
		return g, nil
	}
	if err := c.opts.Durability.Controller(ctx); err != nil {
		g.refusals = append(g.refusals, SweepRefusal{
			Reason: ReasonControllerUnreachable,
			Detail: fmt.Sprintf("the controller could not be reached, and §53 says a peer cut off "+
				"from the control plane does not delete replicas: %v", err),
		})
	}
	return g, nil
}

// establish answers ADR-0018's deferred question for one blob: is this blob
// held, verifiably and recently, somewhere that is not here.
//
// It returns evidence to record, or a [Sparing] explaining why it could not.
// It never returns both nil, and it never returns evidence it did not
// establish — the shape is deliberate, because "no reason to refuse" and "a
// reason to proceed" are different things and only the second may delete.
//
// # Order of checks, and why this order
//
// State, then freshness, then the stored health column, then the peer itself.
// The three cheap checks are all about the CATALOG's belief and can refuse
// without a network round trip; the last one is the only one that produces a
// fact. A peer that passes all three is still asked, because the first three
// are beliefs — see the type comment on Replica.
//
// # The correction
//
// A peer that answers "I do not hold that" has contradicted a `replicas` row
// that says `present`. The row is corrected to `missing` on the way past, so
// the lie does not go on offering the same false assurance to the next sweep,
// to read routing and to replication. It is corrected only when applying: a
// dry run's contract is that it changes nothing, and it still reports the
// refusal.
func (c *Collector) establish(
	ctx context.Context, g gate, b Blob, now time.Time, apply bool,
) (Evidence, *Sparing, error) {
	if len(g.refusals) > 0 {
		r := g.refusals[0]
		return Evidence{}, &Sparing{
			Hash: b.Hash.String(), Size: b.Size, Reason: r.Reason, Detail: r.Detail,
		}, nil
	}
	if g.sole {
		return Evidence{
			BlobHash: b.Hash, Size: b.Size, Basis: BasisSolePeer, RecordedAt: now,
			Detail: "this deployment has exactly one peer, so there is no other placement for the " +
				"policy to be satisfied at; the grace window and the refcount are the whole gate " +
				"(ADR-0010, ADR-0018)",
		}, nil, nil
	}

	replicas, err := c.opts.Catalog.Replicas(ctx, b.Hash)
	if err != nil {
		return Evidence{}, nil, err
	}
	if len(replicas) == 0 {
		return Evidence{}, &Sparing{
			Hash: b.Hash.String(), Size: b.Size, Reason: ReasonNoOtherPeer,
			Detail: "no other peer claims a replica of this blob, so deleting these bytes would " +
				"delete the last copy in the fabric (ADR-0018)",
		}, nil
	}
	// Deterministic: the reason an operator is shown must not depend on map
	// iteration order (ADR-0017).
	sort.Slice(replicas, func(i, j int) bool { return replicas[i].Peer.PeerID < replicas[j].Peer.PeerID })

	var worst *Sparing
	for _, r := range replicas {
		if err := ctx.Err(); err != nil {
			return Evidence{}, nil, err
		}
		sp, err := c.check(ctx, b, r, now, apply)
		if err != nil {
			return Evidence{}, nil, err
		}
		if sp == nil {
			// Established. One verified remote copy is enough — §19's target
			// is that every Full Peer holds everything, and this blob is
			// unreferenced garbage rather than content to be placed.
			return Evidence{
				BlobHash: b.Hash, Size: b.Size, Basis: BasisVerifiedRemote,
				PeerID: r.Peer.PeerID, PeerName: r.Peer.Name, Endpoint: r.Peer.Endpoint,
				ReportedAt: r.ReportedAt, VerifiedAt: r.VerifiedAt, RecordedAt: now,
				Detail: fmt.Sprintf("peer %s answered that it holds these bytes, and last confirmed "+
					"the claim at %s", r.Peer.Name, r.ReportedAt.UTC().Format(time.RFC3339Nano)),
			}, nil, nil
		}
		if worst == nil || rank(sp.Reason) > rank(worst.Reason) {
			worst = sp
		}
	}
	return Evidence{}, worst, nil
}

// check evaluates one claimed replica, and is where the three watched refusals
// actually live.
func (c *Collector) check(
	ctx context.Context, b Blob, r Replica, now time.Time, apply bool,
) (*Sparing, error) {
	spare := func(reason Reason, detail string) *Sparing {
		return &Sparing{
			Hash: b.Hash.String(), Size: b.Size, Reason: reason, Detail: detail,
			PeerID: r.Peer.PeerID, PeerName: r.Peer.Name,
		}
	}

	if !r.Present() {
		return spare(ReasonReplicaNotPresent, fmt.Sprintf(
			"peer %s's replica is %s, which is a claim NOT to hold the bytes", r.Peer.Name, r.State)), nil
	}
	if !r.Fresh(now, c.opts.Freshness) {
		when := "never — no peer has ever confirmed this row in an inventory report"
		if !r.ReportedAt.IsZero() {
			when = r.ReportedAt.UTC().Format(time.RFC3339Nano)
		}
		return spare(ReasonStaleInventory, fmt.Sprintf(
			"peer %s last confirmed this replica at %s, past the %s freshness bound; what is known "+
				"about it is a fact about the past, not a reason to delete the last local copy",
			r.Peer.Name, when, c.opts.Freshness)), nil
	}
	if !r.Peer.Reachable() {
		return spare(ReasonPeerUnreachable, fmt.Sprintf(
			"peer %s is %s, so nothing about where these bytes are can be established",
			r.Peer.Name, health(r.Peer.Health))), nil
	}

	// The claim is checked, not taken. A `replicas` row is the controller's
	// belief and this milestone's premise is that beliefs and bytes diverge.
	err := c.opts.Durability.Holds(ctx, r.Peer, b.Hash)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, ErrPeerLacksBlob):
		if apply {
			if err := c.opts.Catalog.MarkReplicaMissing(ctx, b.Hash, r.Peer.PeerID, now); err != nil {
				return nil, err
			}
		}
		return spare(ReasonRemoteLacksBlob, fmt.Sprintf(
			"peer %s's replica row says present and peer %s answered that it does not hold these "+
				"bytes; the row has been corrected to missing", r.Peer.Name, r.Peer.Name)), nil
	case errors.Is(err, ErrPeerUnreachable):
		return spare(ReasonPeerUnreachable, fmt.Sprintf(
			"peer %s answered nothing when asked whether it holds these bytes: %v", r.Peer.Name, err)), nil
	default:
		return spare(ReasonUnverifiable, fmt.Sprintf(
			"peer %s could not be asked whether it holds these bytes: %v", r.Peer.Name, err)), nil
	}
}

// health renders the stored column for a human, including the case the column
// makes a point of: never heard from is not the same as gone quiet.
func health(state string) string {
	if state == "" || state == "unknown" {
		return "a peer nothing has ever heard from"
	}
	return state
}

// rank orders refusals by how much they tell an operator, so that a blob
// claimed by several peers is reported under the most informative reason
// rather than the alphabetically first.
//
// "That peer is lying to you" outranks "that peer is down", which outranks
// "that peer went quiet", which outranks "that peer already says it does not
// have it". Each step down is a step further from something to go and fix.
func rank(r Reason) int {
	switch r {
	case ReasonRemoteLacksBlob:
		return 5
	case ReasonUnverifiable:
		return 4
	case ReasonPeerUnreachable:
		return 3
	case ReasonStaleInventory:
		return 2
	case ReasonReplicaNotPresent:
		return 1
	case ReasonNoOtherPeer, ReasonDurabilityUnwired, ReasonControllerUnreachable, ReasonCatalogVacuous:
		return 0
	default:
		return 0
	}
}

// vacuity is the non-vacuity guard: it refuses an untracked sweep whose
// catalog has not corroborated that this is the store it belongs to.
//
// out.Considered has existed since M1 to make "garbage collection removed
// nothing" falsifiable, and has never been used as a PRECONDITION. That is the
// gap: the number that proves a sweep looked at something was reported and
// never consulted. Here it is consulted.
func (c *Collector) vacuity(out *Collection, untracked, walked int) *SweepRefusal {
	if untracked < VacuityFloor {
		// Too few for the ratio to mean anything, and squarely the shape of
		// the ingest fault this path exists to clean up (M1-10).
		return nil
	}
	if out.Considered == 0 {
		return &SweepRefusal{
			Reason: ReasonCatalogVacuous,
			Detail: fmt.Sprintf("the catalog knows of no blobs at all and the store holds %d files, "+
				"%d of them untracked and past the grace window. An empty catalog against a "+
				"populated store is a database pointed at the wrong place far more often than "+
				"it is a library that is genuinely gone, and unlinking on that reading is not "+
				"recoverable", walked, untracked),
		}
	}
	if untracked*2 > walked {
		return &SweepRefusal{
			Reason: ReasonCatalogVacuous,
			Detail: fmt.Sprintf("%d of the %d files in the store have no catalog row, against %d "+
				"blobs the catalog considered. More than half a store being untracked is a "+
				"catalog that does not describe this store, not a sweep",
				untracked, walked, out.Considered),
		}
	}
	return nil
}

// spareAll records every untracked candidate as spared under one refusal, so
// that the files a guard protected are named rather than merely counted.
func spareAll(out *Collection, candidates []cas.Descriptor, r SweepRefusal) {
	for _, d := range candidates {
		out.UntrackedSpared = append(out.UntrackedSpared, Sparing{
			Hash: d.Hash.String(), Size: d.Size, Reason: r.Reason, Detail: r.Detail,
		})
	}
}
