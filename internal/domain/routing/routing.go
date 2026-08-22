// Package routing selects which peer a read is served from (spec §31, §32).
//
// §32 lists six inputs — site locality, latency, availability, health,
// bandwidth, transcode capability — and this package implements three of them.
// That is a decision rather than an omission, so it is stated first.
//
// # Three of six, and why the other three are absent
//
// Site locality, availability and health are facts the controller already
// holds: a peer's site is in its membership row, a replica's state is in the
// replicas table, and reachability is M4-10's observed-silence verdict. Latency
// and bandwidth are measurements, and nothing measures them — inventing a
// number here would mean inventing the measurement infrastructure too, in a
// package whose job is to choose rather than to observe. Transcode capability
// is the fleet advertisement of #112, which is not built.
//
// # So this is an ordered preference, not a score
//
// A scoring function with three of six terms stubbed produces a weighting
// nobody chose. Worse, it is hard to change: once "locality is worth 40 points"
// exists, adding latency means re-deciding every other coefficient at the same
// time, and there is no evidence to re-decide them against.
//
// An ordered preference has neither problem. Eligibility is a list of rules
// applied in order; preference is a list of ranked keys compared in order.
// Adding latency later appends one element to preferences and one rule to
// eligibility, and every existing answer is unchanged unless latency is the
// only thing separating two candidates. That is the same shape §63's
// ReleaseCandidate evaluator has, and the same shape the playback planner
// (§68) has: a decision that carries its reasons.
//
// # The reasons are the deliverable
//
// Every candidate that is not selected carries why, and the selected one
// carries why it won. A routing failure that reports "unavailable" and nothing
// else is the outage that takes three hours to diagnose: the operator cannot
// tell a peer that is down from a peer that never had the bytes, and those
// have entirely different fixes.
//
// # A pure function, and nothing else
//
// No database, no clock, no network. The caller assembles candidates from the
// peers table, the replicas table and M4-10's health verdict; this package
// decides. That is what makes the interesting behaviour — locality × health ×
// replica state — table-testable without standing two peers up.
package routing

import (
	"fmt"
	"sort"
)

// Candidate is one peer the controller could route a read to.
//
// It is assembled by the caller from three sources that live in three
// different places, and it is deliberately flat: a candidate carrying a
// database row or a health tracker would make this package impure and make the
// combinatorics untestable.
type Candidate struct {
	PeerID string
	Name   string
	// Site is the peer's failure domain (§35).
	Site string
	// Endpoint is where the peer serves the blob endpoint (ADR-0013). Empty is
	// legitimate for this node — see Source.Endpoint — and disqualifying for
	// any other peer, because a peer nothing can address is not a source.
	Endpoint string
	// IsSelf marks the peer this controller is attached to (ADR-0029).
	IsSelf bool
	// SameSite is whether this peer is at the client's site (§31). The caller
	// resolves it, because "the client's site" is a deployment question rather
	// than a routing one.
	SameSite bool
	// Reachable is M4-10's verdict, as passed through health.Sources. It is a
	// bool rather than a health.State because this package must not import a
	// package that imports database/sql, and because the only question routing
	// asks of health is a yes or a no.
	Reachable bool
	// HealthState is the stored verdict's name, carried for the rejection
	// detail only. "unknown" and "unreachable" are different things to an
	// operator — one has never been heard from, the other has gone quiet — and
	// a rejection that flattened them would send somebody to the wrong machine.
	HealthState string
	// ReplicaState is the replicas row state for the blob being routed, or
	// empty when this peer holds no row at all. Empty and 'missing' are
	// different facts and both are reported as such.
	ReplicaState string
}

// The replica state that may be read from.
//
// 'present' is the schema's word for bytes that are there and good: the CAS
// write path hashes on the way in (invariant 1) and integrity verification
// stamps verified_at on top of that. The gate is deliberately the state and
// NOT `verified_at IS NOT NULL` — ingest records 'present' without a
// verification stamp because the ingest hash IS the verification, and demanding
// a re-verification stamp would make a freshly ingested library unplayable
// until the integrity sweep had walked all of it.
const statePresent = "present"

// Rejection reason codes. Stable strings, because an operator's runbook and a
// client's branch both key on them, and prose changes.
const (
	// RejectNoReplica is a peer that holds no row for these bytes.
	RejectNoReplica = "no_replica"
	// RejectReplicaNotUsable is a peer whose replica exists and cannot be
	// read: pending (still arriving), corrupt (quarantined, ADR-0018) or
	// missing (the bytes went away).
	RejectReplicaNotUsable = "replica_not_usable"
	// RejectPeerUnhealthy is a peer that is not reachable (M4-10).
	RejectPeerUnhealthy = "peer_unhealthy"
	// RejectNoEndpoint is a peer with nowhere to send the client.
	RejectNoEndpoint = "peer_has_no_endpoint"
	// RejectSiteLocalPreferred is an eligible cross-site peer that lost to a
	// same-site one. It is a rejection with no fault in it, and saying so is
	// the point: it is how an operator sees that the fallback existed.
	RejectSiteLocalPreferred = "site_local_preferred"
	// RejectAnotherSourceChosen is an eligible peer that lost the ordered
	// preference to another equally-local one.
	RejectAnotherSourceChosen = "another_source_chosen"
)

// Selection reason codes: why the chosen peer was chosen.
const (
	// SelectedSiteLocal is §31's normal behaviour — a client's site served by
	// its own site's peer.
	SelectedSiteLocal = "site_local"
	// SelectedCrossSiteFallback is §31's fallback, recorded as one. Cross-site
	// streaming "should be fallback behavior, not the norm", and an instance
	// where this reason is usual is an instance where replication is not
	// working. It cannot be noticed unless it is written down.
	SelectedCrossSiteFallback = "cross_site_fallback"
)

// Reason is one contribution to a routing outcome, in the shape §63 uses.
type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Rejection is one peer that was considered and not selected, with every
// reason it was not.
//
// Reasons is a list rather than a single code for the reason
// incompatibleStreams collects all of them: a peer that is both unreachable
// and holds a corrupt replica has two problems, and telling the operator about
// one of them produces a second question after the first fix does not help.
type Rejection struct {
	PeerID  string   `json:"peer_id"`
	Name    string   `json:"name"`
	Site    string   `json:"site"`
	Reasons []Reason `json:"reasons"`
}

// Decision is the router's answer, including when the answer is no.
type Decision struct {
	// Found reports whether a source was selected. False is a refusal, and a
	// refusal is a first-class outcome rather than an error: the client asked
	// where to read from and the honest reply is "nowhere, and here is why for
	// every peer".
	Found bool
	// Source is the selected peer. Zero when Found is false.
	Source Candidate
	// Reason is why Source won. Zero when Found is false.
	Reason Reason
	// Rejected is every other peer considered, in the order they were
	// considered, each with every reason it was not chosen.
	Rejected []Rejection
}

// Refusal renders the whole decision as one line of prose, for a problem
// document and for a log.
//
// It exists because the alternative shipped once: "unavailable", and nothing
// else. The caller that has the structured Rejected list should render that
// instead; this is for the channels that only carry a string.
func (d Decision) Refusal() string {
	if d.Found {
		return ""
	}
	if len(d.Rejected) == 0 {
		return "no peer holds these bytes, and no peer was even considered — " +
			"this deployment has no peers"
	}
	out := "no healthy peer holds these bytes"
	for _, r := range d.Rejected {
		out += "; " + r.Name + " (" + r.Site + "): " + joinDetails(r.Reasons)
	}
	return out
}

func joinDetails(reasons []Reason) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += ", and "
		}
		out += r.Detail
	}
	return out
}

// eligibility is the ordered list of rules a candidate must pass to be a
// source at all.
//
// Ordered so that the reasons read in the order an operator would check them —
// does it have the bytes, are the bytes good, is the machine up, can I reach
// it — and evaluated in full rather than short-circuiting, so a peer that fails
// three of them says so three times.
//
// Adding latency later appends a rule here (a peer past a latency ceiling is
// ineligible) or a preference below (a faster peer wins). It does not touch
// either of the ones already written.
var eligibility = []struct {
	code string
	// fails reports whether the candidate breaks this rule, and says how.
	fails func(Candidate) (string, bool)
}{
	{
		code: RejectNoReplica,
		fails: func(c Candidate) (string, bool) {
			if c.ReplicaState == "" {
				return "the peer holds no replica of these bytes", true
			}
			return "", false
		},
	},
	{
		code: RejectReplicaNotUsable,
		fails: func(c Candidate) (string, bool) {
			if c.ReplicaState != "" && c.ReplicaState != statePresent {
				return fmt.Sprintf("the peer's replica is %s, which cannot be read from",
					c.ReplicaState), true
			}
			return "", false
		},
	},
	{
		code: RejectPeerUnhealthy,
		fails: func(c Candidate) (string, bool) {
			if !c.Reachable {
				return fmt.Sprintf("the peer is %s (M4-10)", healthWord(c.HealthState)), true
			}
			return "", false
		},
	},
	{
		code: RejectNoEndpoint,
		fails: func(c Candidate) (string, bool) {
			// This node needs no endpoint: the client is already connected to
			// it, and a relative URL resolves against the origin it used. Any
			// other peer with no endpoint is a peer the client cannot be sent
			// to, and returning a relative URL for one would silently route
			// the bytes through the controller — the one thing §32 forbids.
			if !c.IsSelf && c.Endpoint == "" {
				return "the peer has no endpoint the client could be sent to", true
			}
			return "", false
		},
	},
}

// healthWord turns a stored health value into something an operator can act
// on. "unknown" and "unreachable" send somebody to different places.
func healthWord(state string) string {
	switch state {
	case "unknown":
		return "unknown — nothing has ever heard from it"
	case "unreachable":
		return "unreachable — it has answered nothing for longer than the health window"
	case "":
		return "of unrecorded health"
	default:
		return state
	}
}

// rank is the ordered preference, as a list of keys compared left to right.
// Lower sorts first.
//
// Element 0 is §31: a same-site peer beats a cross-site one, always, and no
// number of other advantages overturns it. That is what "cross-site streaming
// should be fallback behavior, not the norm" means as code — a preference
// order rather than a weight that something else could outbid.
//
// Element 1 is this node, among peers at the same site. Reading from the disk
// the controller is attached to (ADR-0029) is locality taken to its limit: no
// hop at all. It is a separate element rather than folded into element 0
// because it is a separate claim, and folding it in would make "same site"
// silently mean two things.
//
// Latency, when it exists, becomes element 2. Bandwidth element 3. Neither
// changes an answer these two already separate, which is the property the
// ordering was chosen for.
func rank(c Candidate) []int {
	return []int{boolKey(!c.SameSite), boolKey(!c.IsSelf)}
}

func boolKey(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Select chooses a source (§32).
//
// Candidates are supplied in a stable order by the caller; ties past the
// ordered preference are broken by peer id so that two identical deployments
// route identically and a test asserting a peer id is not asserting a map
// iteration.
func Select(candidates []Candidate) Decision {
	type scored struct {
		candidate Candidate
		keys      []int
		order     int
	}
	var (
		eligible []scored
		decision Decision
	)
	rejectedFor := map[string][]Reason{}

	for i, c := range candidates {
		var reasons []Reason
		for _, rule := range eligibility {
			if detail, failed := rule.fails(c); failed {
				reasons = append(reasons, Reason{Code: rule.code, Detail: detail})
			}
		}
		if len(reasons) > 0 {
			rejectedFor[c.PeerID] = reasons
			continue
		}
		eligible = append(eligible, scored{candidate: c, keys: rank(c), order: i})
	}

	sort.SliceStable(eligible, func(a, b int) bool {
		ka, kb := eligible[a].keys, eligible[b].keys
		for i := range ka {
			if ka[i] != kb[i] {
				return ka[i] < kb[i]
			}
		}
		return eligible[a].candidate.PeerID < eligible[b].candidate.PeerID
	})

	if len(eligible) > 0 {
		winner := eligible[0].candidate
		decision.Found = true
		decision.Source = winner
		decision.Reason = selectionReason(winner)
		for _, loser := range eligible[1:] {
			rejectedFor[loser.candidate.PeerID] = []Reason{runnerUpReason(loser.candidate, winner)}
		}
	}

	// Rejections are emitted in the caller's order rather than the map's, so
	// the response is stable and reviewable.
	for _, c := range candidates {
		reasons, ok := rejectedFor[c.PeerID]
		if !ok {
			continue
		}
		decision.Rejected = append(decision.Rejected, Rejection{
			PeerID: c.PeerID, Name: c.Name, Site: c.Site, Reasons: reasons,
		})
	}
	return decision
}

func selectionReason(c Candidate) Reason {
	if c.SameSite {
		return Reason{
			Code:   SelectedSiteLocal,
			Detail: fmt.Sprintf("the peer is at the client's site (%s), which §31 prefers", c.Site),
		}
	}
	return Reason{
		Code: SelectedCrossSiteFallback,
		Detail: fmt.Sprintf(
			"no peer at the client's site can serve these bytes, so the read falls back to %s at %s; "+
				"§31 says cross-site streaming should be the exception", c.Name, c.Site),
	}
}

func runnerUpReason(loser, winner Candidate) Reason {
	if !loser.SameSite && winner.SameSite {
		return Reason{
			Code: RejectSiteLocalPreferred,
			Detail: fmt.Sprintf("%s is at the client's site and this peer is at %s; §31 prefers the local one",
				winner.Name, loser.Site),
		}
	}
	return Reason{
		Code:   RejectAnotherSourceChosen,
		Detail: fmt.Sprintf("%s was preferred by the ordered selection (§32)", winner.Name),
	}
}
