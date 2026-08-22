package replication

import (
	"errors"
	"fmt"
	"sort"
)

// The transfer itself: which peer the destination pulls from, and what the
// lifecycle of one transfer is called (§21, §32, ADR-0030, M4-09).
//
// # Everything here is a decision, and nothing here moves a byte
//
// The same division the diff above makes. Choosing a source is a decision
// taken from two facts — who is known to hold the bytes, and who is known to
// be up — and it is a pure function of those facts so that it can be asserted
// as one. Opening the connection, presenting a certificate and hashing what
// arrives are infrastructure, and they live in internal/peer/transfer.
//
// # The source is chosen at transfer time and is not in the job
//
// ReplicateBlobPayload deliberately carries no source (see job.go). A payload
// naming one would be a payload that goes stale: the peer that looked best when
// reconciliation ran may be off by the time the transfer starts, and a job
// running against a five-minute-old routing decision is a job operating on a
// guess. So the candidates are read fresh, here, at the moment of the pull.

// Transfer transitions, carried in the payload of
// events.TypeReplicationTransferChanged.
//
// One event type for the whole lifecycle, with the transition in the payload —
// not one type per edge. events.go's own comment gives the reason:
// N event types are N places to forget to emit, and a subscriber wanting only
// failures filters the payload, which it must be able to do anyway.
const (
	// TransferStarted is the destination having chosen a source and begun
	// pulling. The `replicas` row is `pending` from here until the outcome.
	TransferStarted = "started"
	// TransferSucceeded is bytes that arrived, verified against the
	// destination's own expectation, and became a readable blob.
	TransferSucceeded = "succeeded"
	// TransferFailed is every terminal outcome that is not success: no source,
	// a refused source, a connection that died mid-flight, or bytes that did
	// not verify. The reason is in the payload rather than in the type, so
	// that "how did this transfer end" is one field to read.
	TransferFailed = "failed"
)

// Refusals a transfer makes before any bytes move.
var (
	// ErrNoSource is a blob no reachable peer is known to hold. It is not a
	// permanent failure: the peer that holds it may be rebooting, and the job
	// queue retrying is the correct response.
	ErrNoSource = errors.New("replication: no peer this node knows of holds these bytes")
	// ErrSourceNotPinnable is a candidate with no pinned public key.
	//
	// It is refused rather than dialled, and refused HERE rather than at the
	// handshake, because the alternative — connecting to the recorded endpoint
	// and accepting whatever answers — is trust on first use with extra steps.
	// Membership is the only trust root in the inter-peer path (ADR-0012), and
	// a candidate with no key is a candidate membership cannot vouch for.
	ErrSourceNotPinnable = errors.New("replication: this peer has no pinned public key, so there is nothing to authenticate it against")
	// ErrSourceUnreachable is a candidate with no recorded endpoint. A peer is
	// its key and not its address, so this is a configuration gap rather than
	// an identity problem, and it says so separately.
	ErrSourceUnreachable = errors.New("replication: this peer has no recorded endpoint, so there is nowhere to dial")
)

// Health states a candidate may be in, as the peers table records them.
//
// Spelled here rather than imported from internal/peer/health for the reason
// ModeFull is spelled here rather than imported from persistence: this package
// is domain and must not know how a peer is stored (invariant 2). The values
// are the ones migration 00020's CHECK constrains.
const (
	HealthReachable   = "reachable"
	HealthUnknown     = "unknown"
	HealthUnreachable = "unreachable"
)

// Source is a peer the destination may pull a blob from.
//
// It carries the pinned key rather than only the endpoint, because dialling an
// address and trusting whoever answers is the failure ADR-0012 exists to make
// unspellable. Nothing here is a claim the SOURCE made about itself: every
// field is what this node's own membership record says.
type Source struct {
	PeerID string
	Name   string
	// Endpoint is where to dial. Not identity: it may change freely.
	Endpoint string
	// PublicKey is the pinned Ed25519 key this connection must present.
	PublicKey []byte
	// Health is the last thing observed about this peer's reachability. It is
	// advisory here — see RankSources.
	Health string
}

// Usable reports whether this candidate can be dialled at all, and says which
// half is missing when it cannot.
//
// Both refusals happen before a connection is opened, which is the acceptance
// condition's "refused before any bytes move" stated as code rather than as a
// property of the order two functions happen to be called in.
func (s Source) Usable() error {
	if len(s.PublicKey) == 0 {
		return fmt.Errorf("%w: peer %s", ErrSourceNotPinnable, s.PeerID)
	}
	if s.Endpoint == "" {
		return fmt.Errorf("%w: peer %s", ErrSourceUnreachable, s.PeerID)
	}
	return nil
}

// RankSources orders the peers that hold the bytes, best first.
//
// # It ranks rather than filters, and that is the difference from read routing
//
// health.Sources drops everything that is not reachable, which is right for a
// read: a client is waiting, a healthy peer three feet away holds the same
// bytes, and skipping the machine that has been off since Tuesday costs
// nothing. Replication is not a read. Nobody is waiting, the job queue retries,
// and the cost of trying an unknown peer is one failed connection.
//
// The cost of EXCLUDING it is much larger and much quieter. `unknown` is the
// column default (migration 00020) and is what every peer reads as until
// something has probed it, so a fabric that filtered on reachability would
// refuse to start its first transfer until a health beat had run — and would
// report "no source" while sitting next to a peer holding every byte it wants.
// That is the same shape as the destination-filtering mistake health.go's own
// Destinations comment argues against: a durability gap that reports itself as
// nothing to do.
//
// So health decides ORDER. A peer that answered recently is tried first, a peer
// nobody has heard of is tried after it, and a peer known to be down is tried
// last rather than never — because "known to be down" is a fact with a
// timestamp on it, and the timestamp may be older than the reboot.
//
// The order is total and deterministic (ADR-0017): within a health class,
// candidates are ordered by peer id. Two runs of the same fabric therefore pull
// from the same peer, which is what makes a transfer test assert against a
// named source rather than against whichever one the map walk produced.
func RankSources(candidates []Source) []Source {
	out := make([]Source, 0, len(candidates))
	for _, c := range candidates {
		if c.Usable() != nil {
			// Not a candidate at all: there is no key to pin or nowhere to
			// dial. Dropped here rather than attempted and failed, so that
			// "no source" and "a source that cannot be authenticated" are not
			// reported as the same thing by the caller.
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := healthRank(out[i].Health), healthRank(out[j].Health)
		if li != lj {
			return li < lj
		}
		return out[i].PeerID < out[j].PeerID
	})
	return out
}

// healthRank orders the health states. Unknown sits between reachable and
// unreachable deliberately: it is the absence of evidence, not evidence of
// absence, and the column defaults to it.
func healthRank(state string) int {
	switch state {
	case HealthReachable:
		return 0
	case HealthUnreachable:
		return 2
	default:
		return 1
	}
}
