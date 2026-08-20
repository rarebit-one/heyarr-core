// Package playback owns consumption sessions and the playback planner
// (spec §67, §68).
//
// # One session model for five verbs
//
// §67 lists consumption as watching, listening, reading, continuing and
// queueing, and gives them ONE abstraction: ConsumptionSession. ADR-0024
// records why that is not a simplification imposed on the spec but the spec's
// own claim, and what would make us revisit it.
//
// The tempting alternative is three models, because the progress units differ:
// a media timestamp, a track offset, a page or an EPUB CFI. That difference is
// in ONE field. The state machine, the resume query, the event vocabulary and
// the eventual sync protocol are identical across all of them, and building
// three of each to accommodate one field is how a system ends up with three
// sync protocols to keep consistent.
//
// So progress is a LOCATOR and a UNIT, not a float. A page number is not a
// number of seconds, and storing both as "position" makes every reader guess
// which it has.
//
// # This package touches nothing
//
// No os, no path/filepath, no database/sql, no persistence, no CAS — depguard
// enforces it (§18, ADR-0006/0007). The transitions below are a pure function
// of the current state, which is what makes the whole table testable without a
// database in the way. Persistence is a port the API layer supplies.
package playback

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Verb is what a session is doing (§67).
//
// "continuing" and "queueing" from §67 are deliberately NOT verbs here.
// Continuing is a QUERY — the most recent unfinished session for a principal,
// which is what a "Continue watching" row is — and queueing is an ordered set
// of intended sessions. Modelling them as states would mean a session could be
// "queueing", which is not a thing a playback does; it is a thing a client does
// with a list.
type Verb string

const (
	// VerbWatch covers video: films, episodes.
	VerbWatch Verb = "watch"
	// VerbListen covers audio: music, audiobooks.
	VerbListen Verb = "listen"
	// VerbRead covers publications, which Heyarr serves and never renders (§69).
	VerbRead Verb = "read"
)

// Verbs is every verb, in a stable order.
func Verbs() []Verb { return []Verb{VerbWatch, VerbListen, VerbRead} }

// ParseVerb validates a verb from the wire.
func ParseVerb(s string) (Verb, error) {
	for _, v := range Verbs() {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("verb must be one of watch, listen, read, not %q", s)
}

// State is where a session is in its life.
type State string

const (
	// StateCreated is a session that exists and has not started. It is a
	// distinct state rather than an immediate "playing" because a client asks
	// for a plan before it has anything to play: the gap between "I intend to
	// watch this" and "bytes are moving" is real, and collapsing it makes an
	// abandoned plan indistinguishable from a playback that stalled instantly.
	StateCreated State = "created"
	// StatePlaying means bytes are moving. A reader that has no notion of
	// "playing" passes straight through it to paused on its first page turn.
	StatePlaying State = "playing"
	// StatePaused is a session holding its position without consuming.
	StatePaused State = "paused"
	// StateStopped is deliberate abandonment before the end. The progress is
	// kept: that is what makes "continue watching" possible, and it is the
	// difference between stopping and finishing.
	StateStopped State = "stopped"
	// StateCompleted is reaching the end.
	StateCompleted State = "completed"
)

// States is every state, in lifecycle order.
func States() []State {
	return []State{StateCreated, StatePlaying, StatePaused, StateStopped, StateCompleted}
}

// Terminal reports whether a state accepts no further transitions.
func (s State) Terminal() bool { return s == StateStopped || s == StateCompleted }

// Transition is something that happens to a session.
type Transition string

const (
	// TransitionStart begins consumption.
	TransitionStart Transition = "start"
	// TransitionPause holds position without consuming.
	TransitionPause Transition = "pause"
	// TransitionResume continues from the held position.
	TransitionResume Transition = "resume"
	// TransitionProgress records a new position without changing state. It is
	// still a transition and still emits: "where I am" is a fact about the
	// session that a client following the stream needs.
	TransitionProgress Transition = "progress"
	// TransitionStop abandons before the end, keeping the progress. That is
	// what makes "continue watching" possible.
	TransitionStop Transition = "stop"
	// TransitionComplete reaches the end.
	TransitionComplete Transition = "complete"
)

// Transitions is every transition, in a stable order.
func Transitions() []Transition {
	return []Transition{
		TransitionStart, TransitionPause, TransitionResume,
		TransitionProgress, TransitionStop, TransitionComplete,
	}
}

// ErrIllegalTransition is what an impossible transition produces — resuming a
// completed session, progressing a stopped one.
//
// It is a distinct error rather than a generic one because the API turns it
// into a 409 and everything else into a 500, and a client needs to tell "you
// asked for something that cannot happen" from "we broke".
var ErrIllegalTransition = errors.New("illegal session transition")

// ErrInvalidProgress is what a locator the domain cannot accept produces.
//
// It is a sentinel rather than something the API layer recognises by reading
// the message, because a caller that branches on error TEXT is a caller that
// silently starts returning 500 the day someone rewords a message. Two errors
// leave this package and they mean different things to a client — "you are out
// of date" (409) and "you sent nonsense" (400) — so both are typed.
var ErrInvalidProgress = errors.New("invalid progress")

// table is the state machine, written out in full.
//
// Every legal (state, transition) pair is here and everything else is illegal.
// It is a map rather than a switch so that the tests can enumerate the whole
// space — a transition table with only the legal half tested is half a state
// machine, and the illegal half is the half that turns into a 500.
var table = map[State]map[Transition]State{
	StateCreated: {
		TransitionStart: StatePlaying,
		// Stopping something never started is legitimate: a client that asks
		// for a plan and then changes its mind should close the session rather
		// than leave it open forever.
		TransitionStop: StateStopped,
	},
	StatePlaying: {
		TransitionPause: StatePaused,
		// Progress does not change the state, and that is the point: it is
		// still a transition, it still records, and it still emits.
		TransitionProgress: StatePlaying,
		TransitionStop:     StateStopped,
		TransitionComplete: StateCompleted,
	},
	StatePaused: {
		TransitionResume: StatePlaying,
		// A paused client still reports where it is — a reader turning pages
		// with no notion of "playing" lives here for its whole life.
		TransitionProgress: StatePaused,
		TransitionStop:     StateStopped,
		TransitionComplete: StateCompleted,
	},
	// Terminal. Deliberately empty rather than absent, so that "no legal
	// transitions" is a fact in the table rather than a lookup miss that could
	// equally mean "state not handled".
	StateStopped:   {},
	StateCompleted: {},
}

// Next returns the state a transition leads to.
func Next(from State, t Transition) (State, error) {
	allowed, known := table[from]
	if !known {
		return "", fmt.Errorf("%w: %q is not a state", ErrIllegalTransition, from)
	}
	to, ok := allowed[t]
	if !ok {
		return "", fmt.Errorf("%w: cannot %s a %s session", ErrIllegalTransition, t, from)
	}
	return to, nil
}

// Unit is what a Progress locator measures.
//
// It exists so that a page number and a media timestamp do not have to pretend
// to be the same thing. §67 covers watching, listening and reading with one
// session, and this is the one field where they genuinely differ.
type Unit string

const (
	// UnitSeconds is an offset into a timeline, as a decimal string.
	UnitSeconds Unit = "seconds"
	// UnitPage is a page number in a publication, as a decimal string.
	UnitPage Unit = "page"
	// UnitCFI is an EPUB canonical fragment identifier — a structural pointer
	// into the document, opaque to Heyarr, which does not render EPUBs (§69).
	UnitCFI Unit = "cfi"
)

// Units is every unit, in a stable order.
func Units() []Unit { return []Unit{UnitSeconds, UnitPage, UnitCFI} }

// ParseUnit validates a unit from the wire.
func ParseUnit(s string) (Unit, error) {
	for _, u := range Units() {
		if string(u) == s {
			return u, nil
		}
	}
	return "", fmt.Errorf("unit must be one of seconds, page, cfi, not %q", s)
}

// maxLocator bounds a locator. An EPUB CFI is a structural path and is the
// longest of the three by a wide margin; anything past this is not a position
// in a document.
const maxLocator = 512

// Progress is where a session has reached.
//
// Locator is a STRING even for seconds, and that is deliberate. A CFI is not a
// number, and a schema that stores "position" as a float forces every reader to
// carry a second field saying how to interpret it — which is this struct, with
// the type safety removed.
type Progress struct {
	Locator string `json:"locator"`
	Unit    Unit   `json:"unit"`
}

// Zero reports whether any progress has been recorded.
func (p Progress) Zero() bool { return p.Locator == "" }

// Validate checks a locator against its unit.
func (p Progress) Validate() error {
	if p.Locator == "" {
		return fmt.Errorf("%w: locator is required", ErrInvalidProgress)
	}
	if len(p.Locator) > maxLocator {
		return fmt.Errorf("%w: locator is %d bytes, which is longer than %d and is not a position in anything",
			ErrInvalidProgress, len(p.Locator), maxLocator)
	}
	if strings.ContainsAny(p.Locator, "\x00\n\r") {
		return fmt.Errorf("%w: locator contains a control character", ErrInvalidProgress)
	}
	switch p.Unit {
	case UnitSeconds, UnitPage:
		// Both are decimal. What the number MEANS is not this package's
		// business — it has no opinion on whether 1e9 seconds is a plausible
		// offset, because it does not know how long the asset is.
		if err := decimalish(p.Locator); err != nil {
			return fmt.Errorf("%w: a %s %w", ErrInvalidProgress, p.Unit, err)
		}
	case UnitCFI:
		// Opaque by design. Heyarr does not render EPUBs (§69), so it has no
		// business validating the internals of a pointer into one — only that
		// it is a bounded, printable string.
		if !strings.HasPrefix(p.Locator, "epubcfi(") {
			return fmt.Errorf("%w: a cfi locator must be an epubcfi(...) expression", ErrInvalidProgress)
		}
	default:
		return fmt.Errorf("%w: unit must be one of seconds, page, cfi, not %q", ErrInvalidProgress, p.Unit)
	}
	return nil
}

// decimalish checks a non-negative decimal number without deciding what it
// means.
func decimalish(s string) error {
	dots := 0
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && i > 0 && dots == 0:
			dots++
		default:
			return fmt.Errorf("locator must be a non-negative decimal number, not %q", s)
		}
	}
	if s[len(s)-1] == '.' {
		return fmt.Errorf("locator must be a non-negative decimal number, not %q", s)
	}
	return nil
}

// Session is one consumption of one asset by one device (§67).
type Session struct {
	ID        string
	AssetID   string
	DeviceID  string
	Verb      Verb
	State     State
	Progress  Progress
	CreatedAt time.Time
	UpdatedAt time.Time
	// StartedAt is when the session first entered playing. Nil for a session
	// that was created and abandoned, which is what makes those two cases
	// distinguishable in the history.
	StartedAt *time.Time
	// EndedAt is when it reached a terminal state.
	EndedAt *time.Time
}

// Apply moves a session through a transition and returns the result.
//
// It takes and returns a value rather than mutating, so that a caller cannot
// half-apply a transition and then fail to persist it — the old session is
// still intact and still correct until the new one is written.
func (s Session) Apply(t Transition, at time.Time, progress *Progress) (Session, error) {
	next, err := Next(s.State, t)
	if err != nil {
		return Session{}, err
	}
	if progress != nil {
		if err := progress.Validate(); err != nil {
			return Session{}, err
		}
		s.Progress = *progress
	}

	s.State = next
	s.UpdatedAt = at
	if t == TransitionStart && s.StartedAt == nil {
		started := at
		s.StartedAt = &started
	}
	if next.Terminal() {
		ended := at
		s.EndedAt = &ended
	}
	return s, nil
}

// EventType is the §76 event a transition emits.
//
// Every transition emits one — invariant 7, no exceptions — and the mapping
// lives here rather than at the call site so that adding a transition without
// an event is a compile-time hole rather than a silent omission. They are
// namespaced under playback.*, which §76 already reserves.
func EventType(t Transition) string {
	switch t {
	case TransitionStart:
		return "playback.session.started"
	case TransitionPause:
		return "playback.session.paused"
	case TransitionResume:
		return "playback.session.resumed"
	case TransitionProgress:
		return "playback.session.progressed"
	case TransitionStop:
		return "playback.session.stopped"
	case TransitionComplete:
		return "playback.session.completed"
	default:
		// Unreachable through Apply, which rejects an unknown transition
		// before this is called. It is here so that a new Transition constant
		// added without a case produces an event nobody can subscribe to
		// rather than an empty type the log would reject.
		return "playback.session.unknown"
	}
}
