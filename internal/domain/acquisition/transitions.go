package acquisition

import "fmt"

// Transition is something that happens to an acquisition.
type Transition string

const (
	// TransitionSearch starts looking. A search is a job (invariant 4), so
	// this is the moment one is enqueued rather than the moment it runs.
	TransitionSearch Transition = "search"
	// TransitionCandidatesFound is a search that returned releases. It says
	// nothing about whether any of them was acceptable.
	TransitionCandidatesFound Transition = "candidates_found"
	// TransitionNoCandidates is a search that returned nothing.
	//
	// A normal outcome, not a failure, and it must not fail the job into a
	// retry backoff — that turns an unavailable release into an indexer
	// hammering loop. Backoff belongs on the SCHEDULE, not on the job.
	TransitionNoCandidates Transition = "no_candidates"
	// TransitionSelect chooses a candidate, by the evaluator or by a person.
	// Which of the two it was is recorded alongside, not encoded here: a
	// manual override is the same transition made for a different reason, and
	// giving it its own edge would double the table to record provenance.
	TransitionSelect Transition = "select"
	// TransitionRejectAll is candidates found and none acceptable.
	//
	// It returns to missing, and the twelve explained rejections it leaves
	// behind are the deliverable (§63). A want that goes quiet with no
	// explanation is the failure mode §60 keeps "explainable rejection
	// reasons" to avoid.
	TransitionRejectAll Transition = "reject_all"
	// TransitionQueue is the download client accepting the release.
	TransitionQueue Transition = "queue"
	// TransitionStartDownload is bytes beginning to move.
	TransitionStartDownload Transition = "start_download"
	// TransitionDownloaded is the transfer reporting completion. Note it goes
	// to VERIFYING and not to INGESTING: a claim of completion by a third
	// party is not evidence about bytes (invariant 1).
	TransitionDownloaded Transition = "downloaded"
	// TransitionVerified is Heyarr having hashed the bytes itself.
	TransitionVerified Transition = "verified"
	// TransitionIngested is the bytes being under management.
	TransitionIngested Transition = "ingested"

	// TransitionFail is anything in flight going wrong: the client refused the
	// release, the transfer stalled, the bytes did not hash to what was
	// claimed, ingest could not materialise them.
	//
	// ONE transition rather than one per phase, because the destination and
	// the meaning are identical — the want is exactly as unmet as before — and
	// what actually differs is the REASON, which is data. Six near-identical
	// edges would be six places to forget an event.
	TransitionFail Transition = "fail"
	// TransitionCancel is a person stopping something in flight.
	TransitionCancel Transition = "cancel"
	// TransitionLost is managed bytes going away: the asset was deleted, or a
	// scan found the file gone. It is the edge that makes this machine a loop
	// rather than a funnel.
	TransitionLost Transition = "lost"
)

// Transitions is every transition, in a stable order.
func Transitions() []Transition {
	return []Transition{
		TransitionSearch, TransitionCandidatesFound, TransitionNoCandidates,
		TransitionSelect, TransitionRejectAll, TransitionQueue,
		TransitionStartDownload, TransitionDownloaded, TransitionVerified,
		TransitionIngested, TransitionFail, TransitionCancel, TransitionLost,
	}
}

// ParseTransition validates a transition from the wire.
func ParseTransition(s string) (Transition, error) {
	for _, t := range Transitions() {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("%q is not an acquisition transition", s)
}

// table is the pipeline, written out in full.
//
// Every legal (phase, transition) pair is here and everything else is illegal.
// A map rather than a switch so the tests can enumerate the whole space: a
// transition table with only its legal half tested is half a state machine,
// and the illegal half is the half that turns into a 500.
//
// There is no terminal phase. Idle re-enters SEARCHING for the upgrade
// workflow (§60), so every phase has at least one way out. That is a real
// difference from ConsumptionSession (ADR-0024) and it is deliberate: whether
// a want is finished is a question about the WANT — is it monitored, is its
// profile terminal — and answering it here would put policy in the pipeline.
//
// Note what is NOT encoded here. Every failure edge lands on PhaseIdle, and
// whether that idle state presents as MISSING or AVAILABLE depends on whether
// Heyarr holds bytes — which the table does not know and must not guess. Apply
// carries Managed through unchanged, so a failed upgrade search returns to
// AVAILABLE rather than reporting the library as missing.
var table = map[Phase]map[Transition]Phase{
	PhaseIdle: {
		TransitionSearch: PhaseSearching,
		// The bytes went away — a deleted asset, or a scan finding the file
		// gone. Legal from idle only: while a pipeline is in flight there are
		// no managed bytes to lose that this machine is responsible for.
		TransitionLost: PhaseIdle,
	},
	PhaseSearching: {
		TransitionCandidatesFound: PhaseCandidatesFound,
		// Found nothing. Back to rest, and NOT a failure.
		TransitionNoCandidates: PhaseIdle,
		TransitionFail:         PhaseIdle,
		TransitionCancel:       PhaseIdle,
	},
	PhaseCandidatesFound: {
		TransitionSelect: PhaseSelected,
		// Twelve candidates, none acceptable. The rejections stay.
		TransitionRejectAll: PhaseIdle,
		TransitionFail:      PhaseIdle,
		TransitionCancel:    PhaseIdle,
	},
	PhaseSelected: {
		TransitionQueue:  PhaseQueued,
		TransitionFail:   PhaseIdle,
		TransitionCancel: PhaseIdle,
	},
	PhaseQueued: {
		TransitionStartDownload: PhaseDownloading,
		TransitionFail:          PhaseIdle,
		TransitionCancel:        PhaseIdle,
	},
	PhaseDownloading: {
		TransitionDownloaded: PhaseVerifying,
		TransitionFail:       PhaseIdle,
		TransitionCancel:     PhaseIdle,
	},
	PhaseVerifying: {
		TransitionVerified: PhaseIngesting,
		// Bytes that did not hash to what was claimed. The candidate is marked
		// so the next search does not choose it again — otherwise a bad
		// release becomes an infinite download.
		TransitionFail:   PhaseIdle,
		TransitionCancel: PhaseIdle,
	},
	PhaseIngesting: {
		TransitionIngested: PhaseIdle,
		TransitionFail:     PhaseIdle,
		TransitionCancel:   PhaseIdle,
	},
}

// Advance returns the phase a transition leads to.
func Advance(from Phase, t Transition) (Phase, error) {
	allowed, known := table[from]
	if !known {
		return "", fmt.Errorf("%w: %q is not a phase", ErrIllegalTransition, from)
	}
	to, ok := allowed[t]
	if !ok {
		return "", fmt.Errorf("%w: cannot %s while %s", ErrIllegalTransition, t, from)
	}
	return to, nil
}

// Apply advances a whole state, carrying the satisfaction axes correctly.
//
// The axes are not reset by every transition, and that is the point: a search
// starting does not make content unsatisfied, because the bytes you already
// have are still there and still good enough. Only two transitions touch them,
// and both are transitions about the BYTES rather than about the pipeline.
func (s State) Apply(t Transition) (State, error) {
	next, err := Advance(s.Phase, t)
	if err != nil {
		return s, err
	}
	out := State{Phase: next, Managed: s.Managed, Content: s.Content, Placement: s.Placement}

	switch t {
	case TransitionLost:
		// The bytes are gone. Both answers are now no — not unknown:
		// something did look, and the answer is that there is nothing there.
		out.Managed = false
		out.Content = SatisfactionNot
		out.Placement = SatisfactionNot
	case TransitionIngested:
		// New bytes are under management and nothing has evaluated them yet.
		// Unknown rather than satisfied: whether they meet the profile is
		// §56's question, answered by reconciliation, and assuming yes here is
		// exactly how AVAILABLE and CONTENT_SATISFIED collapse.
		out.Managed = true
		out.Content = SatisfactionUnknown
		out.Placement = SatisfactionUnknown
	}

	if err := out.Validate(); err != nil {
		return s, err
	}
	return out, nil
}

// WithContent records an answer on the content axis (§56).
//
// Separate from Apply because satisfaction is not a pipeline event: it is the
// result of reconciliation, which runs on its own schedule and can change the
// answer without anything having been acquired — a quality profile edit can
// unsatisfy a want that nothing else touched (§57).
func (s State) WithContent(v Satisfaction) (State, error) {
	out := s
	out.Content = v
	if v != SatisfactionSatisfied {
		// Placement is a question about bytes that satisfy. If content stops
		// being satisfied, any placement answer is stale rather than false.
		if out.Placement != SatisfactionUnknown {
			out.Placement = SatisfactionUnknown
		}
	}
	if err := out.Validate(); err != nil {
		return s, err
	}
	return out, nil
}

// WithPlacement records an answer on the placement axis (§56).
//
// ## PROVEN
//
// This carried an UNPROVEN block until Milestone 4: with a target set of one,
// this axis was satisfied the moment content was, and PLACEMENT_CONVERGING —
// the state this whole distinction exists to express — was unreachable outside
// a test with a synthetic peer set.
//
// A second Full Peer now exists and real bytes move between the two (M4-09), so
// this method is called with SatisfactionConverging by a running system rather
// than only by a table test: `make demo` drives a want through
// CONTENT_SATISFIED, PLACEMENT_CONVERGING and FULLY_SATISFIED in that order,
// against a transfer that really happened. The transitions did not change to
// earn that.
//
// A deployment of one peer still reaches FULLY_SATISFIED the moment content is
// satisfied, and still must not be read as evidence that replication works.
// That is reported per response as `unproven` (ADR-0027) rather than asserted
// here, because it is a property of the fabric and not of this transition.
func (s State) WithPlacement(v Satisfaction) (State, error) {
	out := s
	out.Placement = v
	if err := out.Validate(); err != nil {
		return s, err
	}
	return out, nil
}
