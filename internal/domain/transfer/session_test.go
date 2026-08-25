package transfer

import (
	"errors"
	"strings"
	"testing"
)

// ids renders a plan for assertion, so a failure names the order it got rather
// than a struct dump.
func ids(sources []Source) string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func planOf(t *testing.T, s Session) []Source {
	t.Helper()
	got, err := s.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return got
}

// Health outranks kind: a reachable web seed beats a peer that has been off
// since Tuesday.
//
// The input order is deliberately the OPPOSITE of the expected output, and the
// winner is not first in the input — otherwise "ordered correctly" and "kept
// the order it was given" are the same sequence, which is the position-zero
// fixture mistake this repository has found three times.
func TestAReachableWebSeedBeatsAnUnreachablePeer(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "peer-down", Kind: KindPeer, Health: HealthUnreachable},
			{ID: "seed-up", Kind: KindWebSeed, Health: HealthReachable},
		},
	})
	if want := "seed-up,peer-down"; ids(got) != want {
		t.Errorf("plan = %s, want %s — health outranks kind", ids(got), want)
	}
}

// Within one health class, the fabric is preferred to the outside world.
func TestWithinAHealthClassPeerBeatsWebSeedBeatsExternal(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "c-external", Kind: KindExternal, Health: HealthReachable},
			{ID: "b-seed", Kind: KindWebSeed, Health: HealthReachable},
			{ID: "a-peer", Kind: KindPeer, Health: HealthReachable},
		},
	})
	if want := "a-peer,b-seed,c-external"; ids(got) != want {
		t.Errorf("plan = %s, want %s", ids(got), want)
	}
}

// The ids are chosen so that alphabetical order would produce the WRONG answer
// if kind were ignored — otherwise the test above passes for an implementation
// that only sorts by id.
func TestKindIsWhatOrdersThemAndNotTheirNames(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "a-external", Kind: KindExternal, Health: HealthReachable},
			{ID: "z-peer", Kind: KindPeer, Health: HealthReachable},
		},
	})
	if want := "z-peer,a-external"; ids(got) != want {
		t.Errorf("plan = %s, want %s — sorted by name rather than by kind", ids(got), want)
	}
}

// Unknown sits BETWEEN reachable and unreachable, and is still attempted.
//
// It is the column default, so a fabric that treated unknown as unreachable
// would refuse its first transfer while sitting next to a peer holding every
// byte — a durability gap that reports itself as nothing to do.
func TestAnUnknownPeerIsTriedAfterAReachableOneAndBeforeADeadOne(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "c-dead", Kind: KindPeer, Health: HealthUnreachable},
			{ID: "b-unknown", Kind: KindPeer},
			{ID: "a-up", Kind: KindPeer, Health: HealthReachable},
		},
	})
	if want := "a-up,b-unknown,c-dead"; ids(got) != want {
		t.Errorf("plan = %s, want %s", ids(got), want)
	}
}

// ADR-0041: an unreachable source is skipped in the ORDER, never fatal to the
// session. A session with one dead peer and one live one still plans.
func TestADeadSourceDoesNotEndTheSession(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "dead", Kind: KindPeer, Health: HealthUnreachable},
			{ID: "live", Kind: KindPeer, Health: HealthReachable},
		},
	})
	if len(got) != 2 {
		t.Fatalf("plan has %d sources, want both — a dead peer is an ordinary day", len(got))
	}
	if got[0].ID != "live" {
		t.Errorf("plan starts at %s, want live", got[0].ID)
	}
}

// Interactive urgency leaves out sources believed to be down: somebody is
// waiting, and a failed dial costs them the wait.
func TestInteractiveUrgencyDoesNotDialASourceBelievedDown(t *testing.T) {
	s := Session{
		Target:  "blake3:aa",
		Urgency: UrgencyInteractive,
		Sources: []Source{
			{ID: "dead", Kind: KindPeer, Health: HealthUnreachable},
			{ID: "live", Kind: KindPeer, Health: HealthReachable},
			{ID: "unknown", Kind: KindPeer},
		},
	}
	got := planOf(t, s)
	if ids(got) != "live,unknown" {
		t.Errorf("plan = %s, want live,unknown — a dead source is skipped when somebody waits", ids(got))
	}

	// The control. The SAME sources at the default urgency keep the dead one,
	// so this is a property of the urgency and not of the source list.
	s.Urgency = ""
	if got := planOf(t, s); len(got) != 3 {
		t.Errorf("background plan has %d sources, want all 3 — nobody is waiting", len(got))
	}
}

// An interactive session whose only source is down is a session with nothing to
// try, and says so rather than returning an empty plan.
func TestNothingToTryIsAnErrorRatherThanAnEmptyPlan(t *testing.T) {
	_, err := Session{
		Target:  "blake3:aa",
		Urgency: UrgencyInteractive,
		Sources: []Source{{ID: "dead", Kind: KindPeer, Health: HealthUnreachable}},
	}.Plan()
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("err = %v, want ErrNoSource — an empty slice reads as 'tried everything'", err)
	}
}

// A source that cannot be named is dropped before it can be attempted.
func TestASourceWithNoIdentityIsNotPlanned(t *testing.T) {
	got := planOf(t, Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "", Kind: KindPeer, Health: HealthReachable},
			{ID: "nameless-kind", Health: HealthReachable},
			{ID: "real", Kind: KindPeer, Health: HealthReachable},
		},
	})
	if ids(got) != "real" {
		t.Errorf("plan = %s, want only the source that can be named and reported", ids(got))
	}
}

// The order is TOTAL and deterministic: same input, same first attempt, every
// run. Without it a transfer test asserts against whichever source a map walk
// produced.
func TestThePlanIsDeterministic(t *testing.T) {
	s := Session{
		Target: "blake3:aa",
		Sources: []Source{
			{ID: "p2", Kind: KindPeer, Health: HealthReachable},
			{ID: "p1", Kind: KindPeer, Health: HealthReachable},
			{ID: "s1", Kind: KindWebSeed, Health: HealthReachable},
			{ID: "x1", Kind: KindExternal},
		},
	}
	first := ids(planOf(t, s))
	for range 20 {
		if got := ids(planOf(t, s)); got != first {
			t.Fatalf("plan changed between runs: %s then %s", first, got)
		}
	}
	if first != "p1,p2,s1,x1" {
		t.Errorf("plan = %s, want p1,p2,s1,x1", first)
	}
}

// Planning does not mutate the caller's slice. A session is a value and the
// caller may hold the same sources for another one.
func TestPlanningDoesNotReorderTheCallersSources(t *testing.T) {
	sources := []Source{
		{ID: "z", Kind: KindExternal, Health: HealthReachable},
		{ID: "a", Kind: KindPeer, Health: HealthReachable},
	}
	s := Session{Target: "blake3:aa", Sources: sources}
	if got := ids(planOf(t, s)); got != "a,z" {
		t.Fatalf("plan = %s", got)
	}
	if sources[0].ID != "z" {
		t.Errorf("the caller's slice was reordered: now starts with %s", sources[0].ID)
	}
}
