package acquisition

import (
	"sort"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Release-candidate evaluation (§63).
//
// # The precedent is internal/domain/playback/planner.go, and it is a close one
//
// A pure function returning a decision AND its reasons, with stable
// machine-readable codes and human prose detail, exhaustively table-tested.
// Everything that makes the planner good applies here unchanged: no database,
// no filesystem, no subprocess, so the interesting behaviour — the
// combinatorics of candidate attributes × profile rules — is exhaustively
// testable rather than mostly untested.
//
// # The rejection reasons ARE the deliverable
//
// §61 lists opaque scoring among the things Heyarr avoids and §60 retains
// explainable rejection reasons among the things it keeps. A score with no
// reasons is exactly the opaque scoring §61 names. "Why did it not grab this
// release" is the acquisition-side twin of "why is my television transcoding
// this", and a scorer that answers only with a number cannot answer it.
//
// So an Evaluation carries a reason for every rule that was CONSIDERED, not
// only for the ones that failed. A rule that passed silently is a rule nobody
// can confirm ran.
//
// # Determinism is a property to test, not to assume
//
// Two candidates with identical attributes must produce identical scores, and
// a set must produce a TOTAL ORDER with ties broken by something stable — not
// by map iteration, which Go will happily randomise and which would produce a
// different acquisition on every run (ADR-0017). That is the single most
// likely defect here and the least likely to be noticed, because a randomly
// ordered tie looks exactly like a working system.
//
// # This package scores candidates it is handed
//
// Where they come from is the provider registry's business and must not be
// visible here. A ReleaseCandidate is a VALUE, and this package compiles and
// tests with no provider, no registry and no network in the module graph —
// which is what lets the whole desired-state lane proceed independently of
// Prowlarr.

// Attributes is what is known about a release.
//
// A missing key means "could not be determined", which is DIFFERENT from a
// zero value and must stay so. M2's HDR detection is a substring match on the
// ffprobe profile and is a recorded weakness; a release title is worse
// evidence, not better. A provider that cannot tell must leave the attribute
// out, so evaluation can report "could not determine" rather than confidently
// reporting false.
type Attributes map[policy.Attribute]policy.Value

// ReleaseCandidate is one release a provider offered.
type ReleaseCandidate struct {
	// ID is stable for a given release from a given provider. It is what makes
	// the tie-break deterministic, so it must not be a position in a response.
	ID string
	// Title is what the indexer called it. Never parsed here — attributes are
	// extracted by the provider (M3-09), and re-deriving them from the title
	// at scoring time would be a second, invisible extraction.
	Title string
	// Provider is which registry entry offered it.
	Provider string
	// Attributes is what could be determined about the release.
	Attributes Attributes
}

// Result is what happened to one rule.
type Result string

const (
	// ResultPass is an accept or terminal rule that held.
	ResultPass Result = "pass"
	// ResultFail is an accept rule that did not hold.
	//
	// It means exactly one thing — THIS GATE REJECTED THE CANDIDATE — and
	// nothing else uses it. A terminal condition that does not hold is a
	// ResultMiss, not a fail: it means "keep looking", not "reject". Letting
	// the two share a word made RejectedBy() report four rejections for a
	// candidate that two gates had rejected, which is how a list of reasons
	// stops being answerable.
	ResultFail Result = "fail"
	// ResultBonus is a preference that was met and contributed its weight.
	ResultBonus Result = "bonus"
	// ResultPenalty is a preference met with a negative weight.
	ResultPenalty Result = "penalty"
	// ResultMiss is a rule that was not met and is NOT a rejection: a
	// preference that did not land, or a terminal condition not reached.
	//
	// Both mean "this could be better", and neither stops the candidate being
	// acquired — §62's `prefer` is a score and never a gate, and `terminal` is
	// a stop condition whose absence simply means the search continues.
	ResultMiss Result = "miss"
	// ResultUndetermined is a rule whose attribute the provider could not
	// determine.
	//
	// It is a distinct result rather than a fail, and the difference matters:
	// "this release is 720p and you wanted 1080p" and "nobody could tell what
	// resolution this is" are different situations, and an operator seeing the
	// second knows to look at the provider rather than at the release.
	ResultUndetermined Result = "undetermined"
)

// Reason is one rule's contribution to an evaluation (§63).
type Reason struct {
	// Rule is the machine-readable identity of the rule: attribute and
	// comparison. Clients branch on this; a client branching on prose is one
	// that breaks when the prose improves.
	Rule string `json:"rule"`
	// Section is which of §62's three the rule came from.
	Section string `json:"section"`
	Result  Result `json:"result"`
	// Score is the contribution, present only for preferences that landed.
	Score int `json:"score,omitempty"`
	// Detail is the prose half, for a human.
	Detail string `json:"detail"`
}

// Evaluation is §63's answer.
type Evaluation struct {
	CandidateID string `json:"candidate_id"`
	// Accepted is whether every gate held. A candidate that is not accepted is
	// never acquired, whatever it scored.
	Accepted bool `json:"accepted"`
	// Score is the sum of the preferences that landed. It is meaningful only
	// among accepted candidates: a rejected one's score is not a near miss, it
	// is a number about something that will not be used.
	Score int `json:"score"`
	// Terminal reports that this candidate meets every terminal condition, so
	// the upgrade workflow has nothing left to want (§62).
	//
	// Computed here rather than by the upgrade workflow so that the two cannot
	// drift: one implementation of "is this as good as it gets".
	Terminal bool `json:"terminal"`
	// Reasons is every rule that was considered, in a stable order.
	Reasons []Reason `json:"reasons"`
}

// Reason returns the first reason for the named rule, if any.
func (e Evaluation) Reason(rule string) (Reason, bool) {
	for _, r := range e.Reasons {
		if r.Rule == rule {
			return r, true
		}
	}
	return Reason{}, false
}

// RejectedBy returns the gates that rejected this candidate.
//
// Scoped to the accept section as well as to ResultFail. Only accept rules can
// produce a fail today, so the section check cannot currently fire — it is
// kept because "rejected by" has one meaning and a future result that reused
// the word would silently widen it.
func (e Evaluation) RejectedBy() []Reason {
	var out []Reason
	for _, r := range e.Reasons {
		if r.Result == ResultFail && r.Section == string(policy.SectionAccept) {
			out = append(out, r)
		}
	}
	return out
}

// ruleID is a rule's stable machine-readable identity.
func ruleID(r policy.Rule) string {
	return string(r.Attribute) + "." + string(r.Op)
}

// Evaluate scores one candidate against one profile (§63).
//
// Deterministic: the same inputs always produce the same output, including the
// order of the reasons.
func Evaluate(c ReleaseCandidate, p policy.Profile) Evaluation {
	e := Evaluation{CandidateID: c.ID, Accepted: true}

	// §62's three sections in order, so the reasons read the way an operator
	// reads the profile.
	for _, sr := range p.Rules() {
		rule := sr.Rule
		id := ruleID(rule)
		actual, known := c.Attributes[rule.Attribute]

		if !known {
			// Undetermined. What that MEANS differs per section, and getting
			// it wrong in either direction is a real bug.
			switch sr.Section {
			case policy.SectionAccept:
				// A gate whose attribute is unknown cannot be shown to hold,
				// and a gate that cannot be shown to hold must not pass. The
				// alternative — assume it passes — silently accepts releases
				// nobody could verify, which is how a profile that says
				// "1080p minimum" ends up with a 480p rip.
				e.Accepted = false
				e.Reasons = append(e.Reasons, Reason{
					Rule: id, Section: string(sr.Section), Result: ResultUndetermined,
					Detail: "the provider could not determine " + string(rule.Attribute) +
						", so this gate cannot be shown to hold",
				})
			case policy.SectionTerminal:
				// A terminal condition that cannot be shown to hold does not
				// hold — so the want stays upgradable, which is the safe
				// direction: it keeps looking rather than stopping early.
				e.Reasons = append(e.Reasons, Reason{
					Rule: id, Section: string(sr.Section), Result: ResultUndetermined,
					Detail: "the provider could not determine " + string(rule.Attribute) +
						", so this cannot be treated as fully satisfying",
				})
			default:
				// A preference that cannot be evaluated contributes nothing.
				// Not a rejection — §62's prefer is never a gate.
				e.Reasons = append(e.Reasons, Reason{
					Rule: id, Section: string(sr.Section), Result: ResultUndetermined,
					Detail: "the provider could not determine " + string(rule.Attribute),
				})
			}
			continue
		}

		held := holds(actual, rule)
		switch sr.Section {
		case policy.SectionAccept:
			if held {
				e.Reasons = append(e.Reasons, Reason{
					Rule: id, Section: string(sr.Section), Result: ResultPass,
					Detail: describe(rule, actual, true),
				})
				continue
			}
			// A failed gate short-circuits the VERDICT but not the reporting:
			// every remaining rule is still evaluated, because an operator
			// asking why a release was rejected wants the whole picture rather
			// than the first thing that went wrong.
			e.Accepted = false
			e.Reasons = append(e.Reasons, Reason{
				Rule: id, Section: string(sr.Section), Result: ResultFail,
				Detail: describe(rule, actual, false),
			})
		case policy.SectionPrefer:
			if !held {
				e.Reasons = append(e.Reasons, Reason{
					Rule: id, Section: string(sr.Section), Result: ResultMiss,
					Detail: describe(rule, actual, false),
				})
				continue
			}
			result := ResultBonus
			if rule.Weight < 0 {
				result = ResultPenalty
			}
			e.Score += rule.Weight
			e.Reasons = append(e.Reasons, Reason{
				Rule: id, Section: string(sr.Section), Result: result,
				Score: rule.Weight, Detail: describe(rule, actual, true),
			})
		default:
			// Terminal. A condition that does not hold is a MISS, not a fail:
			// it means "keep looking", and calling it a failure would put it
			// in RejectedBy() alongside the gates that actually rejected.
			result := ResultMiss
			if held {
				result = ResultPass
			}
			e.Reasons = append(e.Reasons, Reason{
				Rule: id, Section: string(sr.Section),
				Result: result, Detail: describe(rule, actual, held),
			})
		}
	}

	// Terminal is ALL terminal rules holding, as §62's example means them: an
	// operator writing "resolution 2160" and "source remux" means both. A
	// profile with no terminal rules is never terminal — "never stop looking"
	// is a legal and meaningful thing to want, and vacuous truth over an empty
	// set would turn it into "stop immediately".
	e.Terminal = p.HasTerminal() && allTerminalRulesHeld(e)

	// A rejected candidate is never terminal, whatever it scores. Terminality
	// says "this is as good as it gets and we are done"; saying that about
	// something that will not be acquired is meaningless.
	if !e.Accepted {
		e.Terminal = false
	}
	return e
}

func allTerminalRulesHeld(e Evaluation) bool {
	var seen bool
	for _, r := range e.Reasons {
		if r.Section != string(policy.SectionTerminal) {
			continue
		}
		seen = true
		if r.Result != ResultPass {
			return false
		}
	}
	return seen
}

// holds reports whether a rule is satisfied by an actual value.
func holds(actual policy.Value, rule policy.Rule) bool {
	want := rule.Value
	if actual.Kind != want.Kind {
		// A provider that supplied the wrong kind is a provider bug, and the
		// safe reading is that the rule does not hold rather than that it
		// does.
		return false
	}
	switch rule.Op {
	case policy.OpEq:
		return equalValues(actual, want)
	case policy.OpNeq:
		return !equalValues(actual, want)
	case policy.OpGTE:
		return actual.Num >= want.Num
	case policy.OpLTE:
		return actual.Num <= want.Num
	case policy.OpIn:
		return inSet(want.Texts, actual.Text)
	case policy.OpNotIn:
		return !inSet(want.Texts, actual.Text)
	}
	return false
}

func equalValues(a, b policy.Value) bool {
	switch a.Kind {
	case policy.KindInt:
		return a.Num == b.Num
	case policy.KindFlag:
		return a.Flag == b.Flag
	case policy.KindText:
		return a.Text == b.Text
	}
	return false
}

func inSet(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// describe renders the prose half of a reason.
func describe(rule policy.Rule, actual policy.Value, held bool) string {
	verb := "is not"
	if held {
		verb = "is"
	}
	var b strings.Builder
	b.WriteString(string(rule.Attribute))
	b.WriteString(" ")
	b.WriteString(actual.String())
	b.WriteString(", which ")
	b.WriteString(verb)
	b.WriteString(" ")
	b.WriteString(opProse(rule.Op))
	b.WriteString(" ")
	b.WriteString(rule.Value.String())
	return b.String()
}

func opProse(op policy.Op) string {
	switch op {
	case policy.OpEq:
		return "equal to"
	case policy.OpNeq:
		return "different from"
	case policy.OpGTE:
		return "at least"
	case policy.OpLTE:
		return "at most"
	case policy.OpIn:
		return "one of"
	case policy.OpNotIn:
		return "outside"
	}
	return string(op)
}

// Ranked is an evaluated candidate, ready to be ordered.
type Ranked struct {
	Candidate  ReleaseCandidate
	Evaluation Evaluation
}

// EvaluateAll scores a set of candidates and returns them in a TOTAL,
// DETERMINISTIC order: best first.
//
// The ordering is the part that will silently break. Go randomises map
// iteration, and a set of candidates that arrives from a map — or is sorted
// with an unstable comparison that leaves ties in input order — produces a
// different acquisition on every run, which looks exactly like a working
// system. So:
//
//   - accepted candidates always rank above rejected ones, because a rejected
//     candidate's score is a number about something that will not be used;
//   - then by score, descending;
//   - then by candidate ID, ascending, which is a stable key that does not
//     depend on the order the provider returned things in.
//
// The ID tie-break is what makes this a TOTAL order rather than a partial one.
// Without it, two equally good releases swap places between runs and the
// upgrade workflow churns between them forever — a bug that presents as
// bandwidth.
func EvaluateAll(candidates []ReleaseCandidate, p policy.Profile) []Ranked {
	out := make([]Ranked, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, Ranked{Candidate: c, Evaluation: Evaluate(c, p)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Evaluation.Accepted != b.Evaluation.Accepted {
			return a.Evaluation.Accepted
		}
		if a.Evaluation.Score != b.Evaluation.Score {
			return a.Evaluation.Score > b.Evaluation.Score
		}
		return a.Candidate.ID < b.Candidate.ID
	})
	return out
}

// Best returns the highest-ranked ACCEPTED candidate, if any.
//
// A separate function rather than out[0] because the first element of a ranked
// list is not necessarily acceptable — when every candidate was rejected it is
// merely the least bad, and acquiring it would be exactly the behaviour §62's
// gates exist to prevent.
func Best(ranked []Ranked) (Ranked, bool) {
	for _, r := range ranked {
		if r.Evaluation.Accepted {
			return r, true
		}
	}
	return Ranked{}, false
}
