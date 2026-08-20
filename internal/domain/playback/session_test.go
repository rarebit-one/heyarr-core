package playback_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

var at = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// The whole (state, transition) space, legal and illegal.
//
// It is enumerated rather than spot-checked because a transition table with
// only the legal half tested is half a state machine — and the illegal half is
// the half that becomes a 500 in production, or worse, a session that resumes
// after it completed.
//
// legal is written out here rather than derived from the package, because a
// test that asks the code what it does and then asserts it does that is a test
// that passes forever.
func TestEveryStateTransitionPair(t *testing.T) {
	legal := map[playback.State]map[playback.Transition]playback.State{
		playback.StateCreated: {
			playback.TransitionStart: playback.StatePlaying,
			playback.TransitionStop:  playback.StateStopped,
		},
		playback.StatePlaying: {
			playback.TransitionPause:    playback.StatePaused,
			playback.TransitionProgress: playback.StatePlaying,
			playback.TransitionStop:     playback.StateStopped,
			playback.TransitionComplete: playback.StateCompleted,
		},
		playback.StatePaused: {
			playback.TransitionResume:   playback.StatePlaying,
			playback.TransitionProgress: playback.StatePaused,
			playback.TransitionStop:     playback.StateStopped,
			playback.TransitionComplete: playback.StateCompleted,
		},
		playback.StateStopped:   {},
		playback.StateCompleted: {},
	}

	pairs := 0
	for _, from := range playback.States() {
		for _, tr := range playback.Transitions() {
			pairs++
			want, ok := legal[from][tr]
			got, err := playback.Next(from, tr)

			if !ok {
				if err == nil {
					t.Errorf("%s + %s = %s, but that must be illegal", from, tr, got)
					continue
				}
				if !errors.Is(err, playback.ErrIllegalTransition) {
					t.Errorf("%s + %s failed with %v, want ErrIllegalTransition", from, tr, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s + %s failed: %v", from, tr, err)
				continue
			}
			if got != want {
				t.Errorf("%s + %s = %s, want %s", from, tr, got, want)
			}
		}
	}
	if pairs != 30 {
		t.Errorf("covered %d pairs, want 5 states × 6 transitions = 30", pairs)
	}
}

// The two that matter most in prose, because they are the ones a client will
// actually try: a completed session must not resume, and a stopped one must not
// keep recording progress.
func TestTerminalSessionsAcceptNothing(t *testing.T) {
	for _, state := range []playback.State{playback.StateStopped, playback.StateCompleted} {
		s := playback.Session{ID: "s1", State: state}
		for _, tr := range playback.Transitions() {
			if _, err := s.Apply(tr, at, nil); !errors.Is(err, playback.ErrIllegalTransition) {
				t.Errorf("a %s session accepted %s (err=%v)", state, tr, err)
			}
		}
		if !state.Terminal() {
			t.Errorf("%s does not report itself terminal", state)
		}
	}
}

// A rejected transition must leave the session untouched. Apply returns a value
// rather than mutating precisely so a caller cannot half-apply and then fail to
// persist, and this is that promise asserted rather than assumed.
func TestARejectedTransitionChangesNothing(t *testing.T) {
	before := playback.Session{
		ID: "s1", State: playback.StateCompleted,
		Progress: playback.Progress{Locator: "42", Unit: playback.UnitPage},
	}
	after, err := before.Apply(playback.TransitionResume, at,
		&playback.Progress{Locator: "99", Unit: playback.UnitPage})
	if err == nil {
		t.Fatal("resuming a completed session was accepted")
	}
	if after.ID != "" {
		t.Errorf("a rejected transition returned a session: %+v", after)
	}
	if before.Progress.Locator != "42" || before.State != playback.StateCompleted {
		t.Errorf("the original session was mutated: %+v", before)
	}
}

// Timestamps: started_at on the first start and never again, ended_at on
// reaching a terminal state. A session created and abandoned must be
// distinguishable from one that played and stopped, or the history cannot tell
// "nobody watched this" from "someone watched two minutes".
func TestLifecycleTimestamps(t *testing.T) {
	s := playback.Session{ID: "s1", State: playback.StateCreated, CreatedAt: at}

	if s.StartedAt != nil || s.EndedAt != nil {
		t.Fatal("a new session already has lifecycle timestamps")
	}

	started, err := s.Apply(playback.TransitionStart, at.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if started.StartedAt == nil || !started.StartedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("started_at = %v", started.StartedAt)
	}

	// Pause and resume must not move started_at: it is when this session first
	// began, not when it last began.
	paused, err := started.Apply(playback.TransitionPause, at.Add(2*time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := paused.Apply(playback.TransitionResume, at.Add(3*time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.StartedAt.Equal(*started.StartedAt) {
		t.Errorf("started_at moved on resume: %v then %v", started.StartedAt, resumed.StartedAt)
	}
	if resumed.EndedAt != nil {
		t.Error("a resumed session has an end time")
	}

	done, err := resumed.Apply(playback.TransitionComplete, at.Add(4*time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if done.EndedAt == nil || !done.EndedAt.Equal(at.Add(4*time.Minute)) {
		t.Errorf("ended_at = %v", done.EndedAt)
	}

	// Abandoned before starting: no started_at, but an ended_at.
	abandoned, err := s.Apply(playback.TransitionStop, at.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.StartedAt != nil {
		t.Error("a session stopped before it started has a start time")
	}
	if abandoned.EndedAt == nil {
		t.Error("a stopped session has no end time")
	}
}

// One session model, three progress units — the claim ADR-0024 rests on. If a
// page locator could not round-trip through the same session a timestamp does,
// the single model would be a fiction.
func TestProgressCarriesEveryUnitUnchanged(t *testing.T) {
	for _, p := range []playback.Progress{
		{Locator: "1284.5", Unit: playback.UnitSeconds},
		{Locator: "42", Unit: playback.UnitPage},
		{Locator: "epubcfi(/6/14[chap05ref]!/4[body01]/10[para05]/3:10)", Unit: playback.UnitCFI},
	} {
		t.Run(string(p.Unit), func(t *testing.T) {
			s := playback.Session{ID: "s1", State: playback.StatePlaying}
			got, err := s.Apply(playback.TransitionProgress, at, &p)
			if err != nil {
				t.Fatal(err)
			}
			if got.Progress != p {
				t.Errorf("progress = %+v, want %+v", got.Progress, p)
			}
			if got.State != playback.StatePlaying {
				t.Errorf("recording progress changed the state to %s", got.State)
			}
		})
	}
}

func TestProgressValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    playback.Progress
		want string
	}{
		{"empty locator", playback.Progress{Unit: playback.UnitSeconds}, "locator is required"},
		{"unknown unit", playback.Progress{Locator: "1", Unit: "furlongs"}, "unit must be one of"},
		{"seconds must be decimal", playback.Progress{Locator: "1:23", Unit: playback.UnitSeconds}, "decimal number"},
		{"negative seconds", playback.Progress{Locator: "-5", Unit: playback.UnitSeconds}, "decimal number"},
		{"trailing dot", playback.Progress{Locator: "12.", Unit: playback.UnitSeconds}, "decimal number"},
		{"two dots", playback.Progress{Locator: "1.2.3", Unit: playback.UnitSeconds}, "decimal number"},
		{"page must be decimal", playback.Progress{Locator: "iv", Unit: playback.UnitPage}, "decimal number"},
		{"a cfi must be a cfi", playback.Progress{Locator: "chapter 5", Unit: playback.UnitCFI}, "epubcfi"},
		{
			"an unbounded locator",
			playback.Progress{Locator: strings.Repeat("9", 1000), Unit: playback.UnitSeconds},
			"longer than",
		},
		{
			"a control character",
			playback.Progress{Locator: "epubcfi(/6/14)\n", Unit: playback.UnitCFI},
			"control character",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", tc.p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Invariant 7 has no exceptions, so every transition must name an event. A
// transition added without one would emit "" and the log rejects an empty type
// — which is a runtime failure in a write path, discovered by whoever is
// watching the stream rather than by whoever added the transition.
func TestEveryTransitionNamesAnEvent(t *testing.T) {
	seen := map[string]playback.Transition{}
	for _, tr := range playback.Transitions() {
		got := playback.EventType(tr)
		if got == "" || got == "playback.session.unknown" {
			t.Errorf("%s emits %q", tr, got)
			continue
		}
		if !strings.HasPrefix(got, "playback.") {
			t.Errorf("%s emits %q, which is outside the playback.* category §76 reserves", tr, got)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s both emit %q; a subscriber cannot tell them apart", tr, other, got)
		}
		seen[got] = tr
	}
}

func TestParseVerbAndUnitRejectNonsense(t *testing.T) {
	if _, err := playback.ParseVerb("skim"); err == nil {
		t.Error("skim was accepted as a verb")
	}
	if _, err := playback.ParseUnit("furlongs"); err == nil {
		t.Error("furlongs was accepted as a unit")
	}
	for _, v := range playback.Verbs() {
		if _, err := playback.ParseVerb(string(v)); err != nil {
			t.Errorf("%s is a verb but does not parse: %v", v, err)
		}
	}
	for _, u := range playback.Units() {
		if _, err := playback.ParseUnit(string(u)); err != nil {
			t.Errorf("%s is a unit but does not parse: %v", u, err)
		}
	}
}

// Two errors leave this package and they mean different things to a client:
// ErrIllegalTransition becomes a 409 ("you are out of date") and
// ErrInvalidProgress becomes a 400 ("you sent nonsense"). Both must be
// recognisable by errors.Is, because a layer that branches on error TEXT
// starts returning 500 the day someone rewords a sentence and nothing catches
// it.
func TestTheTwoErrorsAreTypedNotMatchedOnText(t *testing.T) {
	_, err := playback.Next(playback.StateCompleted, playback.TransitionResume)
	if !errors.Is(err, playback.ErrIllegalTransition) {
		t.Errorf("an illegal transition is not ErrIllegalTransition: %v", err)
	}
	if errors.Is(err, playback.ErrInvalidProgress) {
		t.Error("an illegal transition also reports as invalid progress")
	}

	for _, p := range []playback.Progress{
		{Unit: playback.UnitSeconds},
		{Locator: "1", Unit: "furlongs"},
		{Locator: "iv", Unit: playback.UnitPage},
		{Locator: "chapter 5", Unit: playback.UnitCFI},
		{Locator: strings.Repeat("9", 1000), Unit: playback.UnitSeconds},
	} {
		err := p.Validate()
		if !errors.Is(err, playback.ErrInvalidProgress) {
			t.Errorf("%+v produced %v, which is not ErrInvalidProgress", p, err)
		}
		if errors.Is(err, playback.ErrIllegalTransition) {
			t.Errorf("%+v reports as an illegal transition", p)
		}
	}

	// And through Apply, which is the path the API actually takes.
	s := playback.Session{ID: "s1", State: playback.StatePlaying}
	_, err = s.Apply(playback.TransitionProgress, at, &playback.Progress{Locator: "x", Unit: playback.UnitPage})
	if !errors.Is(err, playback.ErrInvalidProgress) {
		t.Errorf("Apply lost the error type: %v", err)
	}
}
