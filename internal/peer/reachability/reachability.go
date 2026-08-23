// Package reachability decides whether a peer pairing can actually replicate
// (#186, ADR-0037).
//
// # The asymmetry this package exists for
//
// Two flows are required for one blob to move between two nodes, and they run
// in OPPOSITE directions:
//
//   - inventory is PUSHED, peer → controller (POST /peer/v1/inventory). It is
//     how a controller learns that the other node holds the bytes at all.
//   - bytes are PULLED, destination → source (GET /peer/v1/blobs/{h}/content,
//     ADR-0030). It is how they move.
//
// On a network that carries traffic both ways, that asymmetry is invisible.
// On a one-way network exactly one of the two runs the wrong way, whichever
// node is the destination, and replication deadlocks — silently, because a
// controller that was never told the other node holds a blob correctly emits
// no work for it. Heyarr therefore REQUIRES mutual reachability between
// peers, and this package is where that requirement is checked while an
// operator is still looking at a terminal.
//
// # What is observable, and what is not
//
// The outbound direction is observable directly: dial the peer. The return
// direction is not — a node cannot observe an attempt that never leaves the
// other machine — so it is obtained by ASKING, over the outbound connection
// (GET /peer/v1/reachback), and the answer is only available when the
// outbound direction already works.
//
// That is why the decision table below has three outcomes and not four. A
// pairing where the outbound direction fails is UNPROVEN rather than refused:
// a peer that is powered off, still booting, or not yet enrolled at the other
// end looks exactly the same from here, and refusing enrolment because a
// machine is currently down would be a far more common failure than the one
// this check is for. The refusal is reserved for the case that is genuinely
// diagnostic: this node reached the peer, and the peer, when asked, could not
// reach back.
package reachability

import "fmt"

// Direction is one leg of a pairing, named from THIS node's point of view.
type Direction string

const (
	// DirectionOutbound is this node → the peer. It carries the byte pull
	// when this node is the destination.
	DirectionOutbound Direction = "outbound"
	// DirectionReturn is the peer → this node. It carries the peer's
	// inventory report, and its job claims.
	DirectionReturn Direction = "return"
)

// Result is what one leg was observed to be.
type Result string

const (
	// ResultReachable is an answer received. Any answer: the question is
	// whether packets arrive, not whether the far end is well — the same rule
	// internal/peer/health applies to a probe, and for the same reason.
	ResultReachable Result = "reachable"
	// ResultUnreachable is silence, or a refused connection, where an attempt
	// was actually made.
	ResultUnreachable Result = "unreachable"
	// ResultUnknown is no attempt, or an attempt whose failure says nothing
	// about the network — no endpoint recorded, a node that does not serve
	// the reachback route, a local identity that could not be loaded.
	//
	// It is a distinct value rather than folded into unreachable because the
	// two call for opposite decisions: unreachable in the return direction is
	// the refusal this package exists to make, and unknown must never be.
	ResultUnknown Result = "unknown"
)

// Verdict is what a pairing's two legs mean together.
type Verdict string

const (
	// VerdictBidirectional is both legs observed reachable — the topology
	// Heyarr supports, and the only one in which replication can complete in
	// both directions.
	VerdictBidirectional Verdict = "bidirectional"
	// VerdictReturnPathUnreachable is this node reaching the peer while the
	// peer cannot reach back. This is #186's deadlock, observed rather than
	// inferred, and it is the one verdict that refuses an enrolment.
	VerdictReturnPathUnreachable Verdict = "return_path_unreachable"
	// VerdictUnproven is everything else: nothing was demonstrated, in either
	// direction, and a check that cannot see is not evidence of a fault.
	VerdictUnproven Verdict = "unproven"
)

// Decide is the whole decision, as a pure function.
//
// It is deliberately not "refuse unless both are reachable". That version
// refuses every peer that happens to be rebooting, which is a far commoner
// event than a one-way network, and an operator who is refused for the wrong
// reason learns to pass the escape hatch every time.
func Decide(outbound, ret Result) Verdict {
	switch {
	case outbound == ResultReachable && ret == ResultReachable:
		return VerdictBidirectional
	case outbound == ResultReachable && ret == ResultUnreachable:
		return VerdictReturnPathUnreachable
	default:
		return VerdictUnproven
	}
}

// Refuses reports whether this verdict blocks an enrolment.
func (v Verdict) Refuses() bool { return v == VerdictReturnPathUnreachable }

// Pairing is one checked pairing, and what was observed about each leg.
type Pairing struct {
	// PeerName and Endpoint are what the operator typed, echoed back so the
	// refusal names the values they can change.
	PeerName string
	Endpoint string
	// Outbound and Return are the two legs.
	Outbound Result
	Return   Result
	// ReturnTarget is the address the PEER tried, which it reads out of its
	// own membership record for this node. It is the single most useful line
	// in the refusal: a peer that tried a stale address for this node has a
	// configuration problem, not a network one.
	ReturnTarget string
	// Detail is whatever the failing leg reported, verbatim.
	Detail string
}

// Verdict is what this pairing's legs mean.
func (p Pairing) Verdict() Verdict { return Decide(p.Outbound, p.Return) }

// Failed names the direction that did not work, or the empty direction when
// nothing failed conclusively.
func (p Pairing) Failed() Direction {
	if p.Verdict() == VerdictReturnPathUnreachable {
		return DirectionReturn
	}
	return ""
}

// Refusal is the error an enrolment fails with, or nil.
//
// It names the direction, both addresses, and the two flows that need it, in
// that order — an operator meeting this has to be able to act on it without
// reading an ADR, and "reachability check failed" is not something anyone can
// act on.
func (p Pairing) Refusal() error {
	if !p.Verdict().Refuses() {
		return nil
	}
	target := p.ReturnTarget
	if target == "" {
		target = "this node's endpoint as recorded there"
	}
	detail := ""
	if p.Detail != "" {
		detail = "\nIt reported: " + p.Detail
	}
	return fmt.Errorf(
		"this pairing cannot replicate, so %s was not enrolled.\n"+
			"This node reached %s at %s, and %s could not reach back to %s: the %s direction is dead.%s\n"+
			"Replication needs both. A peer's inventory report travels peer → controller, and that is "+
			"how a controller learns the peer holds a blob at all; the bytes then travel destination → "+
			"source (ADR-0030). One direction carries one of them, and the other flow deadlocks — "+
			"silently, as a reconciliation that correctly emits nothing (#186, ADR-0037).\n"+
			"Fix the return path — a firewall rule, a NAT forward, an endpoint recorded there that is "+
			"no longer this node's address — and run this again. "+
			"If the return path exists but cannot be demonstrated from here, "+
			"`--skip-reachability-check` enrols the peer anyway",
		p.PeerName, p.PeerName, p.Endpoint, p.PeerName, target, DirectionReturn, detail)
}
