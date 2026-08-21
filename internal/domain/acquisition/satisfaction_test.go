package acquisition

import (
	"errors"
	"strings"
	"testing"
)

// inFlight is a state mid-pipeline: nothing held, nothing evaluated.
func inFlight(p Phase) State {
	return State{Phase: p, Content: SatisfactionUnknown, Placement: SatisfactionUnknown}
}

// The two axes, and §64's twelve names derived from them (ADR-0027).
//
// This is the file that stops CONTENT_SATISFIED and FULLY_SATISFIED from
// collapsing into each other — the one outcome the milestone epic explicitly
// names.

// Every §64 name is reachable, and each from exactly the state that should
// produce it. A name nothing can reach is a state machine that cannot express
// what the spec drew.
func TestEverySpecNameIsReachable(t *testing.T) {
	available := func(content, placement Satisfaction) State {
		return State{Phase: PhaseIdle, Managed: true, Content: content, Placement: placement}
	}

	cases := []struct {
		name  string
		state State
	}{
		{"MISSING", Initial()},
		{"SEARCHING", inFlight(PhaseSearching)},
		{"CANDIDATES_FOUND", inFlight(PhaseCandidatesFound)},
		{"SELECTED", inFlight(PhaseSelected)},
		{"QUEUED", inFlight(PhaseQueued)},
		{"DOWNLOADING", inFlight(PhaseDownloading)},
		{"VERIFYING", inFlight(PhaseVerifying)},
		{"INGESTING", inFlight(PhaseIngesting)},

		// The four that share a phase. This is the whole point.
		{"AVAILABLE", available(SatisfactionUnknown, SatisfactionUnknown)},
		{"CONTENT_SATISFIED", available(SatisfactionSatisfied, SatisfactionUnknown)},
		{"PLACEMENT_CONVERGING", available(SatisfactionSatisfied, SatisfactionConverging)},
		{"FULLY_SATISFIED", available(SatisfactionSatisfied, SatisfactionSatisfied)},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.state.Validate(); err != nil {
				t.Fatalf("this state should be legal: %v", err)
			}
			if got := tc.state.Name(); got != tc.name {
				t.Fatalf("Name() = %s, want %s", got, tc.name)
			}
		})
		seen[tc.name] = true
	}
	if len(seen) != 12 {
		t.Fatalf("§64 draws twelve names; %d are covered here", len(seen))
	}
}

// The collapse this design exists to prevent, asserted directly: bytes that are
// good enough and bytes that are everywhere they should be are DIFFERENT
// answers, and one cannot be read off the other.
func TestContentSatisfiedIsNotFullySatisfied(t *testing.T) {
	content := State{
		Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionUnknown,
	}
	full := State{
		Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied,
	}

	if content.Name() == full.Name() {
		t.Fatal("obtaining usable content and replicating it to every required peer " +
			"are different questions and must not be collapsed (§56)")
	}
	if content.Name() != "CONTENT_SATISFIED" || full.Name() != "FULLY_SATISFIED" {
		t.Fatalf("got %s and %s", content.Name(), full.Name())
	}

	// And placement can regress without content moving at all — a peer going
	// away long after the bytes arrived. An ordinal counting 0..11 cannot
	// express this without moving backwards through states that mean
	// something else.
	regressed, err := full.WithPlacement(SatisfactionConverging)
	if err != nil {
		t.Fatal(err)
	}
	if regressed.Content != SatisfactionSatisfied {
		t.Error("placement regressing must not disturb the content answer")
	}
	if regressed.Name() != "PLACEMENT_CONVERGING" {
		t.Errorf("Name() = %s, want PLACEMENT_CONVERGING", regressed.Name())
	}
}

// AVAILABLE and CONTENT_SATISFIED are also different, and for a reason that is
// easy to lose: bytes being under management says nothing about whether they
// are good enough. A 480p rip under a 1080p-minimum profile is available and
// not satisfied, and conflating the two makes the upgrade workflow unreachable.
func TestAvailableIsNotContentSatisfied(t *testing.T) {
	unevaluated := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionUnknown, Placement: SatisfactionUnknown}
	rejected := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionNot, Placement: SatisfactionUnknown}
	satisfied := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionUnknown}

	if unevaluated.Name() != "AVAILABLE" || rejected.Name() != "AVAILABLE" {
		t.Errorf("bytes not known to be good enough are AVAILABLE, got %s and %s",
			unevaluated.Name(), rejected.Name())
	}
	if satisfied.Name() != "CONTENT_SATISFIED" {
		t.Errorf("Name() = %s, want CONTENT_SATISFIED", satisfied.Name())
	}

	// "Nobody has looked" and "we looked and the answer is no" are different,
	// and both are AVAILABLE by §64's vocabulary — but they must stay tellable
	// apart underneath, because they lead to different actions.
	if unevaluated.Content == rejected.Content {
		t.Error("unknown and not_satisfied must stay distinct: a fresh want and one " +
			"just found wanting need different handling")
	}
}

// The impossible combination, refused explicitly rather than made
// unrepresentable — making it unrepresentable means one enum of legal pairs,
// which is the ordinal ADR-0027 exists to reject.
func TestImpossibleStatesAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		state State
		want  string
	}{
		{
			"placed but not obtained",
			State{Phase: PhaseIdle, Managed: true, Content: SatisfactionNot, Placement: SatisfactionSatisfied},
			"before there are bytes",
		},
		{
			"converging but not obtained",
			State{Phase: PhaseIdle, Managed: true, Content: SatisfactionUnknown, Placement: SatisfactionConverging},
			"before there are bytes",
		},
		{
			"content satisfied while Heyarr holds nothing",
			State{Phase: PhaseIdle, Managed: false, Content: SatisfactionSatisfied, Placement: SatisfactionUnknown},
			"statement about managed bytes",
		},
		{
			"a phase that is not a phase",
			State{Phase: "nearly", Content: SatisfactionUnknown, Placement: SatisfactionUnknown},
			"not an acquisition phase",
		},
		{
			"converging content",
			State{Phase: PhaseIdle, Managed: true, Content: SatisfactionConverging, Placement: SatisfactionUnknown},
			"not a content satisfaction",
		},
		{
			"content not applicable",
			State{Phase: PhaseIdle, Managed: true, Content: SatisfactionNotApplicable, Placement: SatisfactionUnknown},
			"not a content satisfaction",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.state.Validate()
			if err == nil {
				t.Fatal("this state should be refused")
			}
			if !errors.Is(err, ErrImpossibleState) {
				t.Errorf("should be ErrImpossibleState, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should explain %q, said: %v", tc.want, err)
			}
		})
	}
}

// ADR-0020's blob-less asset, and its fifth special case. A linked asset can
// satisfy content — it is playable, and telling an operator to re-acquire
// something they already have is wrong — but it can never satisfy placement,
// because there is nothing to replicate.
//
// Calling that "satisfied" because zero required blobs are all present would be
// vacuously true and a lie by construction: FULLY_SATISFIED would mean "one
// copy, on one disk, with no integrity guarantee".
func TestALinkedAssetIsContentSatisfiedAndNeverFullySatisfied(t *testing.T) {
	linked := State{
		Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionNotApplicable,
	}
	if err := linked.Validate(); err != nil {
		t.Fatalf("a linked asset is a legal thing to have: %v", err)
	}
	if got := linked.Name(); got != "CONTENT_SATISFIED" {
		t.Fatalf("Name() = %s — a linked asset's honest end state is CONTENT_SATISFIED, "+
			"and it needs no new name", got)
	}
	if linked.Name() == "FULLY_SATISFIED" {
		t.Fatal("a linked asset has no blob (ADR-0020), so it can never be fully satisfied")
	}
}

// The axes are not reset by every transition. A search starting does not make
// content unsatisfied: the bytes you already have are still there and still
// good enough, which is exactly the situation the upgrade workflow runs in.
func TestSearchingForAnUpgradeKeepsWhatYouAlreadyHave(t *testing.T) {
	satisfied := State{
		Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied,
	}
	searching, err := satisfied.Apply(TransitionSearch)
	if err != nil {
		t.Fatal(err)
	}
	if searching.Phase != PhaseSearching {
		t.Fatalf("phase = %s, want searching", searching.Phase)
	}
	if searching.Content != SatisfactionSatisfied {
		t.Error("looking for something better must not un-satisfy what you have — " +
			"otherwise every upgrade scan reports the library as missing")
	}
	if searching.Placement != SatisfactionSatisfied {
		t.Error("looking for something better must not disturb placement either")
	}
}

// Ingesting produces bytes nobody has evaluated. Assuming they satisfy is
// exactly how AVAILABLE and CONTENT_SATISFIED collapse.
func TestIngestingLeavesSatisfactionUnknown(t *testing.T) {
	s := inFlight(PhaseIngesting)
	after, err := s.Apply(TransitionIngested)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != SatisfactionUnknown {
		t.Errorf("content = %s after ingest; whether new bytes meet the profile is "+
			"§56's question, answered by reconciliation, not assumed here", after.Content)
	}
	if after.Name() != "AVAILABLE" {
		t.Errorf("Name() = %s, want AVAILABLE", after.Name())
	}
}

// Losing the bytes answers both axes with "no" — not "unknown". Something did
// look, and the answer is that there is nothing there.
func TestLosingTheBytesUnsatisfiesBothAxes(t *testing.T) {
	full := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied}
	lost, err := full.Apply(TransitionLost)
	if err != nil {
		t.Fatal(err)
	}
	if lost.Phase != PhaseIdle || lost.Managed {
		t.Fatalf("state = (%s, managed=%v), want (idle, managed=false)", lost.Phase, lost.Managed)
	}
	if lost.Content != SatisfactionNot || lost.Placement != SatisfactionNot {
		t.Errorf("both axes should read not_satisfied, got %s and %s", lost.Content, lost.Placement)
	}
	if lost.Name() != "MISSING" {
		t.Errorf("Name() = %s, want MISSING", lost.Name())
	}
}

// Content ceasing to be satisfied makes any placement answer stale rather than
// false: placement is a question about bytes that satisfy, so with none there
// is no answer to keep.
func TestUnsatisfyingContentClearsPlacement(t *testing.T) {
	full := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied}
	after, err := full.WithContent(SatisfactionNot)
	if err != nil {
		t.Fatalf("a profile edit can unsatisfy a want that nothing else touched (§57): %v", err)
	}
	if after.Placement != SatisfactionUnknown {
		t.Errorf("placement = %s; it should be unknown once content no longer satisfies", after.Placement)
	}
	if after.Name() != "AVAILABLE" {
		t.Errorf("Name() = %s, want AVAILABLE", after.Name())
	}
}

// An illegal axis change leaves the state untouched. A mutator that half-applies
// and then reports an error is worse than one that refuses.
func TestARefusedChangeLeavesTheStateAlone(t *testing.T) {
	before := inFlight(PhaseDownloading)
	after, err := before.WithContent(SatisfactionSatisfied)
	if err == nil {
		t.Fatal("content cannot be satisfied while the pipeline is downloading")
	}
	if after != before {
		t.Errorf("a refused change mutated the state: %+v became %+v", before, after)
	}

	after, err = before.Apply(TransitionIngested)
	if err == nil {
		t.Fatal("downloading cannot ingest")
	}
	if after != before {
		t.Errorf("a refused transition mutated the state: %+v became %+v", before, after)
	}
}

// Initial() is a real resting state, not a zero value that happens to work.
func TestInitialIsValidAndMissing(t *testing.T) {
	s := Initial()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Name() != "MISSING" {
		t.Errorf("Name() = %s, want MISSING", s.Name())
	}
	// The zero State is NOT valid, which is what stops an uninitialised struct
	// from passing for a fresh want.
	if err := (State{}).Validate(); err == nil {
		t.Error("the zero value must not validate; it would make a forgotten " +
			"initialisation look like a legitimate MISSING")
	}
}

// The value sets differ per axis, and the difference is asserted rather than
// left as a comment.
func TestAxisValueSetsDiffer(t *testing.T) {
	for _, v := range []Satisfaction{SatisfactionConverging, SatisfactionNotApplicable} {
		s := State{Phase: PhaseIdle, Managed: true, Content: v, Placement: SatisfactionUnknown}
		if err := s.Validate(); err == nil {
			t.Errorf("%s is not a content answer", v)
		}
	}
	for _, v := range PlacementValues() {
		s := State{Phase: PhaseIdle, Managed: true, Content: SatisfactionSatisfied, Placement: v}
		if err := s.Validate(); err != nil {
			t.Errorf("%s should be a legal placement answer: %v", v, err)
		}
	}
}

// The bug the first version of this package had, kept as a regression test.
//
// MISSING and AVAILABLE were modelled as two PHASES. A monitored want whose
// content already satisfies re-enters SEARCHING for an upgrade (§60); when
// that search came back with nothing, the transition table sent it to
// PhaseMissing — because a (phase, transition) table cannot know whether
// Heyarr is holding bytes. Every fruitless upgrade scan therefore reported a
// perfectly good library as missing, and the next pass would try to acquire
// what was already on disk.
//
// The fix is the same move ADR-0027 makes for the satisfaction axes: "do we
// hold bytes" is a separate fact, and MISSING versus AVAILABLE is derived.
func TestAFruitlessUpgradeSearchDoesNotLoseTheLibrary(t *testing.T) {
	full := State{
		Phase: PhaseIdle, Managed: true,
		Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied,
	}
	if full.Name() != "FULLY_SATISFIED" {
		t.Fatalf("setup: Name() = %s", full.Name())
	}

	searching, err := full.Apply(TransitionSearch)
	if err != nil {
		t.Fatal(err)
	}
	if !searching.Managed {
		t.Error("searching for an upgrade must not forget that bytes are held")
	}

	// Every way a search can end without acquiring anything.
	for _, ending := range []Transition{
		TransitionNoCandidates, TransitionFail, TransitionCancel,
	} {
		t.Run(string(ending), func(t *testing.T) {
			after, err := searching.Apply(ending)
			if err != nil {
				t.Fatal(err)
			}
			if after.Name() == "MISSING" {
				t.Fatalf("a fruitless upgrade search reported the library as MISSING — "+
					"the want still holds satisfying bytes (state: %+v)", after)
			}
			if after.Name() != "FULLY_SATISFIED" {
				t.Errorf("Name() = %s, want FULLY_SATISFIED — nothing about what is "+
					"held changed", after.Name())
			}
		})
	}

	// And the same from CANDIDATES_FOUND, where every candidate is rejected —
	// the twelve-rejections case, which is the most likely real outcome of an
	// upgrade scan on a good library.
	found, err := searching.Apply(TransitionCandidatesFound)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := found.Apply(TransitionRejectAll)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Name() != "FULLY_SATISFIED" {
		t.Errorf("rejecting every upgrade candidate left the want %s, want FULLY_SATISFIED",
			rejected.Name())
	}
}

// The mirror case: a want that holds nothing must NOT come back from a failed
// search claiming to be available.
func TestAFailedFirstSearchStaysMissing(t *testing.T) {
	fresh := Initial()
	searching, err := fresh.Apply(TransitionSearch)
	if err != nil {
		t.Fatal(err)
	}
	for _, ending := range []Transition{TransitionNoCandidates, TransitionFail, TransitionCancel} {
		after, err := searching.Apply(ending)
		if err != nil {
			t.Fatal(err)
		}
		if after.Name() != "MISSING" {
			t.Errorf("%s left a want holding nothing as %s, want MISSING", ending, after.Name())
		}
	}
}

// The full happy path, end to end, as a single walk. It is the sequence every
// other test cuts a slice out of, and having it written once makes the shape
// of the pipeline readable.
func TestTheHappyPathReachesFullySatisfied(t *testing.T) {
	s := Initial()
	if s.Name() != "MISSING" {
		t.Fatalf("start: %s", s.Name())
	}

	for _, step := range []struct {
		transition Transition
		want       string
	}{
		{TransitionSearch, "SEARCHING"},
		{TransitionCandidatesFound, "CANDIDATES_FOUND"},
		{TransitionSelect, "SELECTED"},
		{TransitionQueue, "QUEUED"},
		{TransitionStartDownload, "DOWNLOADING"},
		{TransitionDownloaded, "VERIFYING"},
		{TransitionVerified, "INGESTING"},
		// Ingested lands on AVAILABLE and NOT on CONTENT_SATISFIED: nothing has
		// evaluated the new bytes against the profile yet.
		{TransitionIngested, "AVAILABLE"},
	} {
		next, err := s.Apply(step.transition)
		if err != nil {
			t.Fatalf("%s: %v", step.transition, err)
		}
		if got := next.Name(); got != step.want {
			t.Fatalf("after %s: Name() = %s, want %s", step.transition, got, step.want)
		}
		s = next
	}

	// Reconciliation answers the two questions, separately, in that order.
	var err error
	if s, err = s.WithContent(SatisfactionSatisfied); err != nil {
		t.Fatal(err)
	}
	if s.Name() != "CONTENT_SATISFIED" {
		t.Fatalf("Name() = %s, want CONTENT_SATISFIED", s.Name())
	}
	if s, err = s.WithPlacement(SatisfactionConverging); err != nil {
		t.Fatal(err)
	}
	if s.Name() != "PLACEMENT_CONVERGING" {
		t.Fatalf("Name() = %s, want PLACEMENT_CONVERGING", s.Name())
	}
	if s, err = s.WithPlacement(SatisfactionSatisfied); err != nil {
		t.Fatal(err)
	}
	if s.Name() != "FULLY_SATISFIED" {
		t.Fatalf("Name() = %s, want FULLY_SATISFIED", s.Name())
	}
}
