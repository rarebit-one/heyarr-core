package acquisition

import (
	"testing"
	"time"
)

// The search cadence policy (#130).
//
// Everything here is pure, which is the point of putting the policy in the
// domain: "a want that keeps finding nothing is searched less often" is a
// table test rather than something to watch a log for over an afternoon.

// The load-bearing claim of decision 3: TWO schedules, not one with a flag.
//
// If this ever passes with the two collapsed, the assertion is wrong rather
// than the design: it must fail on Base AND on Max, because a collapse that
// left one of them different would still be one schedule with a knob.
func TestTheTwoSchedulesAreDifferentSchedules(t *testing.T) {
	missing, upgrade := MissingSearches(), UpgradeSearches()

	if missing.Name == upgrade.Name {
		t.Fatalf("both schedules are named %q; they are stored by name", missing.Name)
	}
	if upgrade.Base <= missing.Base {
		t.Errorf("upgrade base %s is not slower than missing base %s — "+
			"searching for a better copy of something that is already fine must be "+
			"the rarer question (§60)", upgrade.Base, missing.Base)
	}
	if upgrade.Max <= missing.Max {
		t.Errorf("upgrade ceiling %s is not slower than missing ceiling %s", upgrade.Max, missing.Max)
	}
	// Not a stylistic preference: an order of magnitude is what makes these two
	// policies rather than two settings of one.
	if upgrade.Base < 10*missing.Base {
		t.Errorf("upgrade base %s is less than ten times missing base %s; that is a knob, "+
			"not a second schedule", upgrade.Base, missing.Base)
	}
}

func TestScheduleForMapsStateToPolicy(t *testing.T) {
	satisfied := State{
		Phase: PhaseIdle, Managed: true,
		Content: SatisfactionSatisfied, Placement: SatisfactionSatisfied,
	}
	available := State{
		Phase: PhaseIdle, Managed: true,
		Content: SatisfactionNot, Placement: SatisfactionUnknown,
	}
	missing := Initial()

	cases := []struct {
		name      string
		state     State
		monitored bool
		want      string // the schedule name, or "" for none
	}{
		{"a fresh want nobody has looked for", missing, true, "missing"},
		{"an unmonitored want with nothing held", missing, false, "missing"},
		{"bytes held that the profile refuses", available, true, "missing"},
		{"a satisfied, monitored want", satisfied, true, "upgrade"},
		{"a satisfied, unmonitored want", satisfied, false, ""},
		{"a search already in flight", State{Phase: PhaseSearching}, true, ""},
		{"a download in flight", State{Phase: PhaseDownloading, Managed: false}, true, ""},
		{"ingesting", State{Phase: PhaseIngesting}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ScheduleFor(tc.state, tc.monitored)
			if tc.want == "" {
				if ok {
					t.Fatalf("scheduled %q; this want must not be searched", got.Name)
				}
				return
			}
			if !ok {
				t.Fatal("no schedule; this want must be searched")
			}
			if got.Name != tc.want {
				t.Errorf("schedule = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

// Decision 2: the backoff is ACROSS calls, and it decays to a ceiling rather
// than to silence.
func TestBackoffGrowsAndThenStopsGrowing(t *testing.T) {
	for _, s := range Schedules() {
		t.Run(s.Name, func(t *testing.T) {
			prev := time.Duration(0)
			reachedMax := false
			for n := range 40 {
				d := s.Delay(n)
				if d < prev {
					t.Fatalf("Delay(%d) = %s went backwards from %s", n, d, prev)
				}
				if d > s.Max {
					t.Fatalf("Delay(%d) = %s exceeded the ceiling %s", n, d, s.Max)
				}
				if d == s.Max {
					reachedMax = true
				}
				if n == 0 && d != s.Base {
					t.Fatalf("the first delay is %s, want the base %s", d, s.Base)
				}
				if n > 0 && !reachedMax && d != 2*prev {
					t.Fatalf("Delay(%d) = %s is not double Delay(%d) = %s", n, d, n-1, prev)
				}
				prev = d
			}
			if !reachedMax {
				t.Errorf("the backoff never reached its %s ceiling; a want that is never "+
					"searched again is a want that silently stops meaning anything", s.Max)
			}
			// A want is never abandoned: the ceiling holds forever rather than
			// growing into a delay nobody will live to see.
			if got := s.Delay(1_000_000); got != s.Max {
				t.Errorf("Delay(1000000) = %s, want the ceiling %s", got, s.Max)
			}
		})
	}
}

func TestNegativeFruitlessIsTheBase(t *testing.T) {
	s := MissingSearches()
	if got := s.Delay(-3); got != s.Base {
		t.Errorf("Delay(-3) = %s, want the base %s", got, s.Base)
	}
}

// The spread is what stops forty wants imported in one second from hitting an
// indexer as one burst forever.
func TestSpreadIsDeterministicAndBounded(t *testing.T) {
	s := MissingSearches()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	first := s.NextAt(now, 0, "want-a")
	if again := s.NextAt(now, 0, "want-a"); !again.Equal(first) {
		t.Fatalf("the same want got two different slots, %s then %s — the offset must be "+
			"derived so it can be explained (ADR-0017)", first, again)
	}

	distinct := map[time.Time]bool{}
	for _, id := range []string{"want-a", "want-b", "want-c", "want-d", "want-e"} {
		at := s.NextAt(now, 0, id)
		offset := at.Sub(now) - s.Base
		if offset < 0 || offset >= s.Base/4 {
			t.Errorf("%s: offset %s is outside [0, %s)", id, offset, s.Base/4)
		}
		distinct[at] = true
	}
	if len(distinct) < 2 {
		t.Error("five wants all landed in the same slot; the spread is not spreading")
	}

	// The spread never shortens the interval below the floor. An hour is the
	// floor precisely because the cost of asking too often is a tracker
	// account, not CPU.
	if s.NextAt(now, 0, "want-a").Sub(now) < s.Base {
		t.Error("the spread pulled a search EARLIER than the schedule's base")
	}
}

func TestSpreadWithoutAKeyIsNoOffset(t *testing.T) {
	s := UpgradeSearches()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if got := s.NextAt(now, 0, "").Sub(now); got != s.Base {
		t.Errorf("with no key the next search is %s after now, want exactly the base %s", got, s.Base)
	}
}
