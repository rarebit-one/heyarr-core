package acquisition

import (
	"errors"
	"fmt"
)

// Phase is a position in the acquisition pipeline (§64).
//
// # Why there is no MISSING phase and no AVAILABLE phase
//
// §64 draws both, and they are not pipeline positions. Both mean "nothing is
// in flight" — they differ only in whether Heyarr is holding bytes, which is a
// fact about the LIBRARY rather than about an acquisition.
//
// Modelling them as two phases makes the pipeline lie during an upgrade. A
// monitored want whose content already satisfies re-enters SEARCHING (§60);
// while that search runs, the bytes are still there and still good enough, and
// when it comes back with nothing the want must return to AVAILABLE and not to
// MISSING. A (phase, transition) table cannot decide which of the two to
// return to, because the answer depends on something the pipeline does not
// know. The first version of this file had that bug, and its own upgrade test
// found it.
//
// So there is ONE resting phase, and whether it presents as MISSING or as
// AVAILABLE is derived from Managed. That is the same move ADR-0027 makes for
// the satisfaction axes, applied to a third fact §64's single column also
// flattens.
type Phase string

const (
	// PhaseIdle is "nothing in flight". It presents as MISSING when Heyarr
	// holds no bytes for this want and as AVAILABLE when it does.
	PhaseIdle Phase = "idle"
	// PhaseSearching is a search job in flight against the providers.
	PhaseSearching Phase = "searching"
	// PhaseCandidatesFound is a search that returned releases. It says nothing
	// about whether any was acceptable — that is what evaluation decides, and
	// a search that found twelve unacceptable candidates passes through here
	// on its way back to idle, leaving twelve explained rejections behind.
	PhaseCandidatesFound Phase = "candidates_found"
	// PhaseSelected is one candidate chosen, by the evaluator or by a person.
	PhaseSelected Phase = "selected"
	// PhaseQueued is the download client having accepted it.
	PhaseQueued Phase = "queued"
	// PhaseDownloading is bytes moving, somewhere else, by something that is
	// not Heyarr (§58).
	PhaseDownloading Phase = "downloading"
	// PhaseVerifying is Heyarr hashing what arrived. A real phase doing real
	// work: a download client's checksum is a claim by a third party about
	// bytes it fetched from strangers, and invariant 1 says a destination
	// always verifies bytes itself.
	PhaseVerifying Phase = "verifying"
	// PhaseIngesting is bringing verified bytes under management (§65).
	PhaseIngesting Phase = "ingesting"
)

// Phases is every phase, in pipeline order.
func Phases() []Phase {
	return []Phase{
		PhaseIdle, PhaseSearching, PhaseCandidatesFound, PhaseSelected,
		PhaseQueued, PhaseDownloading, PhaseVerifying, PhaseIngesting,
	}
}

// ParsePhase validates a phase from the wire.
func ParsePhase(s string) (Phase, error) {
	for _, p := range Phases() {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q is not an acquisition phase", s)
}

// InFlight reports whether something is actively happening, which is what
// decides whether a new search may start.
func (p Phase) InFlight() bool { return p != PhaseIdle }

// Satisfaction is one axis's answer (§56).
//
// The two axes share a type because they are the same KIND of question asked
// about different things. They do not share a value set: see contentValues and
// placementValues.
type Satisfaction string

const (
	// SatisfactionUnknown is "not evaluated yet", and is distinct from
	// unsatisfied on purpose: "we looked and the answer is no" and "nobody has
	// looked" lead to different actions, and collapsing them makes a fresh
	// want indistinguishable from one just found wanting.
	SatisfactionUnknown Satisfaction = "unknown"
	// SatisfactionNot is evaluated, and the answer is no.
	SatisfactionNot Satisfaction = "not_satisfied"
	// SatisfactionConverging is placement only: some required peers hold the
	// bytes and some do not, and replication is expected to close the gap.
	SatisfactionConverging Satisfaction = "converging"
	// SatisfactionSatisfied is evaluated, and the answer is yes.
	SatisfactionSatisfied Satisfaction = "satisfied"
	// SatisfactionNotApplicable is placement only, and it exists because of
	// ADR-0020: a `linked` asset has NO blob, so there is nothing to replicate
	// and placement is not a question that can be answered about it.
	//
	// The tempting alternative is to call it satisfied — zero required blobs
	// are all present, which is vacuously true. That would be a lie by
	// construction: FULLY_SATISFIED would then mean "one copy, on one disk,
	// with no integrity guarantee and no way to verify it", which is the
	// opposite of what the name promises. This is the fifth place ADR-0020's
	// blob-less asset needs a special case.
	SatisfactionNotApplicable Satisfaction = "not_applicable"
)

// contentValues and placementValues differ, and the difference is not an
// oversight. "Converging" is meaningless for content — you either hold bytes
// the profile accepts or you do not, there is no partial file that
// half-satisfies a profile. "Not applicable" is meaningless for content,
// because content satisfaction is the whole question a DesiredItem asks.
var (
	contentValues = []Satisfaction{
		SatisfactionUnknown, SatisfactionNot, SatisfactionSatisfied,
	}
	placementValues = []Satisfaction{
		SatisfactionUnknown, SatisfactionNot, SatisfactionConverging,
		SatisfactionSatisfied, SatisfactionNotApplicable,
	}
)

// ContentValues is every answer the content axis may take, in a stable order.
func ContentValues() []Satisfaction { return append([]Satisfaction(nil), contentValues...) }

// PlacementValues is every answer the placement axis may take, in a stable
// order. It is a superset of ContentValues: see the note on the two sets.
func PlacementValues() []Satisfaction { return append([]Satisfaction(nil), placementValues...) }

func valid(set []Satisfaction, v Satisfaction) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// State is an acquisition's whole position: three independent facts that §64
// presents as one column of twelve boxes (ADR-0027).
type State struct {
	// Phase is where the pipeline is.
	Phase Phase
	// Managed is whether Heyarr holds bytes for this want.
	//
	// It is independent of Phase because a want can be acquiring an UPGRADE
	// while already holding a perfectly good copy. Folding it into the phase
	// is what makes a failed upgrade search report the library as missing.
	Managed bool
	// Content answers "do we hold bytes the quality profile accepts?" (§56)
	Content Satisfaction
	// Placement answers "are those bytes on every Full Peer that should hold
	// them?" (§56) — a separate question, and the one that must never be
	// collapsed into the first.
	Placement Satisfaction
}

// Initial is where a DesiredItem starts: nothing held, nothing in flight,
// nothing evaluated.
func Initial() State {
	return State{
		Phase:     PhaseIdle,
		Managed:   false,
		Content:   SatisfactionUnknown,
		Placement: SatisfactionUnknown,
	}
}

// ErrIllegalTransition is what an impossible transition produces.
//
// A distinct error rather than a generic one because the API turns it into a
// 409 and everything else into a 500, and a caller needs to tell "you asked
// for something that cannot happen" from "we broke".
var ErrIllegalTransition = errors.New("illegal acquisition transition")

// ErrImpossibleState is what an incoherent combination of facts produces.
var ErrImpossibleState = errors.New("impossible acquisition state")

// Validate refuses a state that cannot mean anything.
func (s State) Validate() error {
	if _, err := ParsePhase(string(s.Phase)); err != nil {
		return fmt.Errorf("%w: %w", ErrImpossibleState, err)
	}
	if !valid(contentValues, s.Content) {
		return fmt.Errorf("%w: %q is not a content satisfaction — content is unknown, "+
			"not_satisfied or satisfied (a partial file does not half-satisfy a profile)",
			ErrImpossibleState, s.Content)
	}
	if !valid(placementValues, s.Placement) {
		return fmt.Errorf("%w: %q is not a placement satisfaction", ErrImpossibleState, s.Placement)
	}

	// Content satisfaction is a statement about bytes Heyarr HOLDS. Note the
	// constraint is against Managed and NOT against the phase: during an
	// upgrade search a want is simultaneously searching and satisfied, which
	// is the normal case for a monitored library rather than an edge case.
	if s.Content == SatisfactionSatisfied && !s.Managed {
		return fmt.Errorf("%w: content is satisfied while Heyarr holds nothing — "+
			"content satisfaction is a statement about managed bytes (§56)", ErrImpossibleState)
	}

	// The combination §56 forbids, refused explicitly rather than made
	// unrepresentable. Making it unrepresentable would mean one enum of the
	// legal combinations, which is the ordinal ADR-0027 exists to reject: it
	// reads tidily and it is what collapses the axes back into one.
	if s.Placement == SatisfactionSatisfied || s.Placement == SatisfactionConverging {
		if s.Content != SatisfactionSatisfied {
			return fmt.Errorf("%w: placement is %s while content is %s — bytes cannot be "+
				"placed on peers before there are bytes worth placing (§56)",
				ErrImpossibleState, s.Placement, s.Content)
		}
	}
	return nil
}

// Name renders §64's name for this state.
//
// A PRESENTATION of the four fields, computed on demand. It is not stored, and
// nothing branches on it: the moment something does, the axes have a single
// ordinal in front of them again and the collapse is back.
func (s State) Name() string {
	if s.Phase != PhaseIdle {
		return upper(string(s.Phase))
	}
	if !s.Managed {
		return "MISSING"
	}
	if s.Content != SatisfactionSatisfied {
		// Bytes are under management and they are not good enough — or nobody
		// has checked. §64 lists AVAILABLE and CONTENT_SATISFIED separately
		// for exactly this gap: a 480p rip under a 1080p-minimum profile is
		// available and not satisfied, and conflating the two makes the
		// upgrade workflow unreachable.
		return "AVAILABLE"
	}
	switch s.Placement {
	case SatisfactionSatisfied:
		return "FULLY_SATISFIED"
	case SatisfactionConverging:
		return "PLACEMENT_CONVERGING"
	case SatisfactionNotApplicable:
		// A linked asset can never be fully satisfied, because there is
		// nothing to replicate. CONTENT_SATISFIED is the honest end state for
		// it and it needs no new name.
		return "CONTENT_SATISFIED"
	default:
		return "CONTENT_SATISFIED"
	}
}

func upper(s string) string {
	out := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
