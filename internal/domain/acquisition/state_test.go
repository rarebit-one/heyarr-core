package acquisition

import (
	"errors"
	"strings"
	"testing"
)

// The transition table, enumerated in full.
//
// Every legal edge asserted legal and every illegal one asserted illegal. A
// table with only its legal half tested is half a state machine, and the
// illegal half is the half that turns into a 500.

// legalEdges is the expected table, written out independently of the
// implementation. Two lists written by the same person at the same time only
// prove they agree — but this one is compared against the code's OWN table by
// enumeration below, so an edge added to one and not the other fails.
var legalEdges = map[Phase]map[Transition]Phase{
	PhaseIdle: {
		TransitionSearch: PhaseSearching,
		TransitionLost:   PhaseIdle,
	},
	PhaseSearching: {
		TransitionCandidatesFound: PhaseCandidatesFound,
		TransitionNoCandidates:    PhaseIdle,
		TransitionFail:            PhaseIdle,
		TransitionCancel:          PhaseIdle,
	},
	PhaseCandidatesFound: {
		TransitionSelect:    PhaseSelected,
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
		TransitionFail:     PhaseIdle,
		TransitionCancel:   PhaseIdle,
	},
	PhaseIngesting: {
		TransitionIngested: PhaseIdle,
		TransitionFail:     PhaseIdle,
		TransitionCancel:   PhaseIdle,
	},
}

// Every (phase, transition) pair, legal and illegal. This is the whole space —
// 9 phases × 13 transitions = 117 cells — and every one of them is asserted.
func TestTheWholeTransitionSpace(t *testing.T) {
	var legal, illegal int
	for _, phase := range Phases() {
		for _, tr := range Transitions() {
			want, isLegal := legalEdges[phase][tr]
			got, err := Advance(phase, tr)

			switch {
			case isLegal:
				legal++
				if err != nil {
					t.Errorf("%s + %s should be legal, got: %v", phase, tr, err)
					continue
				}
				if got != want {
					t.Errorf("%s + %s = %s, want %s", phase, tr, got, want)
				}
			default:
				illegal++
				if err == nil {
					t.Errorf("%s + %s should be illegal, but reached %s", phase, tr, got)
					continue
				}
				if !errors.Is(err, ErrIllegalTransition) {
					t.Errorf("%s + %s: error should be ErrIllegalTransition, got %v", phase, tr, err)
				}
				// A caller has to be able to tell what it did wrong.
				msg := err.Error()
				if !strings.Contains(msg, string(phase)) || !strings.Contains(msg, string(tr)) {
					t.Errorf("%s + %s: the refusal should name both, got %q", phase, tr, msg)
				}
			}
		}
	}
	// A guard on the guard: if the space collapsed to nothing this test would
	// pass having asserted nothing at all.
	if total := legal + illegal; total != len(Phases())*len(Transitions()) {
		t.Fatalf("the space is %d cells, expected %d", total, len(Phases())*len(Transitions()))
	}
	if legal < 20 || illegal < 70 {
		t.Fatalf("only %d legal and %d illegal edges — the table is not what this test thinks", legal, illegal)
	}
	t.Logf("%d legal edges, %d illegal, %d cells", legal, illegal, legal+illegal)
}

// Nothing is terminal. Every phase has a way out, which is what makes this a
// loop rather than a funnel — the upgrade workflow re-enters from AVAILABLE.
func TestNoPhaseIsTerminal(t *testing.T) {
	for _, phase := range Phases() {
		var ways int
		for _, tr := range Transitions() {
			if _, err := Advance(phase, tr); err == nil {
				ways++
			}
		}
		if ways == 0 {
			t.Errorf("%s has no way out, so an acquisition can get stuck there", phase)
		}
	}
	// Specifically: idle goes back to SEARCHING. Without this edge the
	// upgrade workflow (§60) has nowhere to start.
	if got, err := Advance(PhaseIdle, TransitionSearch); err != nil || got != PhaseSearching {
		t.Errorf("idle + search = (%s, %v), want (searching, nil)", got, err)
	}
}

// Every phase before AVAILABLE can fail, and failing always returns to
// MISSING: a failed acquisition leaves the want exactly as unmet as before.
func TestEverythingInFlightCanFail(t *testing.T) {
	for _, phase := range Phases() {
		if !phase.InFlight() {
			continue
		}
		got, err := Advance(phase, TransitionFail)
		if err != nil {
			t.Errorf("%s cannot fail, so a stall there has nowhere to go: %v", phase, err)
			continue
		}
		if got != PhaseIdle {
			t.Errorf("%s + fail = %s, want idle", phase, got)
		}
	}
	// And the resting phase cannot "fail", because nothing is happening.
	if _, err := Advance(PhaseIdle, TransitionFail); err == nil {
		t.Error("idle should not be failable — nothing is in flight")
	}
}

// A search that finds nothing is a normal outcome with a modelled edge, not a
// failure. If it were a failure the job would back off and an unavailable
// release would become an indexer hammering loop.
func TestAnEmptySearchIsNotAFailure(t *testing.T) {
	got, err := Advance(PhaseSearching, TransitionNoCandidates)
	if err != nil || got != PhaseIdle {
		t.Fatalf("searching + no_candidates = (%s, %v), want (idle, nil)", got, err)
	}
	if _, err := Advance(PhaseIdle, TransitionNoCandidates); err == nil {
		t.Error("no_candidates only means something while searching")
	}
}

// Candidates found and none acceptable is its own edge, distinct from finding
// nothing: twelve explained rejections is a different situation from an empty
// result, and §63's reasons are the deliverable.
func TestRejectingEveryCandidateIsDistinctFromFindingNone(t *testing.T) {
	if _, err := Advance(PhaseCandidatesFound, TransitionRejectAll); err != nil {
		t.Fatalf("candidates_found + reject_all should be legal: %v", err)
	}
	if _, err := Advance(PhaseSearching, TransitionRejectAll); err == nil {
		t.Error("nothing can be rejected before candidates are found")
	}
	if _, err := Advance(PhaseCandidatesFound, TransitionNoCandidates); err == nil {
		t.Error("no_candidates and reject_all are different situations and must not be interchangeable")
	}
}

// Invariant 1: a download client's claim of completion is not evidence about
// bytes. The pipeline routes through VERIFYING and cannot skip it.
func TestADownloadCannotSkipVerification(t *testing.T) {
	got, err := Advance(PhaseDownloading, TransitionDownloaded)
	if err != nil {
		t.Fatal(err)
	}
	if got != PhaseVerifying {
		t.Fatalf("a completed download goes to %s; it must go to verifying (invariant 1)", got)
	}
	if _, err := Advance(PhaseDownloading, TransitionIngested); err == nil {
		t.Error("a download cannot become ingested without Heyarr hashing it itself")
	}
	if _, err := Advance(PhaseDownloading, TransitionVerified); err == nil {
		t.Error("nothing is verified until it has been through verifying")
	}
}
