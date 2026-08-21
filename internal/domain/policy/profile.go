package policy

import (
	"errors"
	"fmt"
	"strings"
)

// Section is which of §62's three parts a rule belongs to.
//
// It is carried on the rule rather than left implicit in which slice the rule
// lives in, because every error message this package produces has to name it —
// "a weight on an accept rule" is actionable and "invalid rule" is not.
type Section string

const (
	// SectionAccept is a gate.
	SectionAccept Section = "accept"
	// SectionPrefer is a score.
	SectionPrefer Section = "prefer"
	// SectionTerminal is a stop condition.
	SectionTerminal Section = "terminal"
)

// Sections lists the three, in §62's order.
func Sections() []Section { return []Section{SectionAccept, SectionPrefer, SectionTerminal} }

// Rule is one assertion about a release.
//
// The same shape serves all three sections. What differs is what a failure
// MEANS — rejection, a forgone bonus, or "keep looking" — and that meaning
// belongs to the section, not to the rule. One rule type is what lets §63
// report all three the same way: an attribute, a comparison, an operand, and
// what happened.
type Rule struct {
	Attribute Attribute `json:"attribute"`
	Op        Op        `json:"op"`
	Value     Value     `json:"value"`
	// Weight is the score contribution, and is meaningful ONLY in `prefer`.
	//
	// It may be negative: a penalty is a real thing to want ("prefer anything
	// that is not a cam"). It may not be zero — a preference that cannot
	// affect the score is indistinguishable from an absent one, and keeping it
	// means an operator believes something is being preferred when nothing is.
	Weight int `json:"weight,omitempty"`
}

// String renders a rule for a human. Used in errors and in §63's reasons.
func (r Rule) String() string {
	return fmt.Sprintf("%s %s %s", r.Attribute, r.Op, r.Value)
}

// Profile is a quality profile (§62).
type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Accept are gates. Every one must pass or the candidate is rejected.
	Accept []Rule `json:"accept"`
	// Prefer are scored. None is required; each contributes its weight.
	Prefer []Rule `json:"prefer"`
	// Terminal are stop conditions. ALL must hold for a candidate to be
	// terminal — an operator who writes "resolution 2160" and "source remux"
	// means both, as §62's example does.
	//
	// Empty means there is no condition under which this profile is finished,
	// which is legal and means "never stop looking". An absent `terminal` and
	// an empty one are the SAME thing on purpose: both say "no condition", and
	// a difference between them would be one no operator could articulate.
	Terminal []Rule `json:"terminal"`
}

// HasTerminal reports whether this profile can ever be finished.
func (p Profile) HasTerminal() bool { return len(p.Terminal) > 0 }

// Rules returns every rule with the section it came from, in a stable order.
//
// Stable so that validation errors, and later §63's reasons, are reported in
// the same order every time. A determinism guarantee that stops at the
// evaluator and leaks at the reporting is not one.
func (p Profile) Rules() []SectionRule {
	out := make([]SectionRule, 0, len(p.Accept)+len(p.Prefer)+len(p.Terminal))
	for _, r := range p.Accept {
		out = append(out, SectionRule{Section: SectionAccept, Rule: r})
	}
	for _, r := range p.Prefer {
		out = append(out, SectionRule{Section: SectionPrefer, Rule: r})
	}
	for _, r := range p.Terminal {
		out = append(out, SectionRule{Section: SectionTerminal, Rule: r})
	}
	return out
}

// SectionRule is a rule and the section it belongs to.
type SectionRule struct {
	Section Section
	Rule    Rule
}

// maxRules bounds one section.
//
// Not a limit anyone meets — a real profile has a handful of rules — but a
// bound on what one write token can make the evaluator iterate over for every
// candidate of every search, forever. The device capability lists are bounded
// for the same reason.
const maxRules = 64

// Validate checks a profile, returning the first problem with enough context
// to fix it.
//
// Validation happens HERE, at write time, and not in the evaluator. A rule
// naming an attribute that does not exist is a mistake in a profile, and a
// mistake in a profile should be reported to whoever wrote the profile — not
// converted into a rejection reason attached to every candidate for the next
// six months, which is where an evaluator-time check would put it.
func (p *Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("a quality profile needs a name")
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Description = strings.TrimSpace(p.Description)

	for _, section := range Sections() {
		rules := p.section(section)
		if len(*rules) > maxRules {
			return fmt.Errorf("the %s section has %d rules, past the limit of %d",
				section, len(*rules), maxRules)
		}
		seen := make(map[string]bool, len(*rules))
		for i := range *rules {
			rule := &(*rules)[i]
			if err := validateRule(section, rule); err != nil {
				return fmt.Errorf("%s rule %d (%s): %w", section, i+1, rule.Attribute, err)
			}
			// An exact duplicate of (attribute, op) within one section is
			// almost always a paste. Contradiction — "resolution gte 1080" and
			// "resolution lte 720" — is deliberately NOT detected: it needs a
			// lattice over every operand type to do properly, and the honest
			// alternative is already good, because §63 reports an
			// unsatisfiable gate as a rejection naming the rule that failed.
			// A profile that rejects everything and says why is legible; a
			// half-implemented contradiction check that misses most cases is
			// not.
			key := string(rule.Attribute) + "/" + string(rule.Op)
			if seen[key] {
				return fmt.Errorf("the %s section names %s %s twice",
					section, rule.Attribute, rule.Op)
			}
			seen[key] = true
		}
	}
	return nil
}

func (p *Profile) section(s Section) *[]Rule {
	switch s {
	case SectionAccept:
		return &p.Accept
	case SectionPrefer:
		return &p.Prefer
	default:
		return &p.Terminal
	}
}

func validateRule(section Section, r *Rule) error {
	r.Attribute = Attribute(normaliseText(string(r.Attribute)))
	r.Op = Op(normaliseText(string(r.Op)))

	kind, known := KindOf(r.Attribute)
	if !known {
		return fmt.Errorf("there is no such attribute — it must be one of %s", attributeNames())
	}
	if r.Op == "" {
		return fmt.Errorf("needs a comparison — one of %s", opNames(kind))
	}
	if !opAllowed(kind, r.Op) {
		return fmt.Errorf("%s is a %s attribute, so %q does not apply to it — use one of %s",
			r.Attribute, kind, r.Op, opNames(kind))
	}
	if !r.Value.IsSet() {
		return errors.New("needs a value to compare against")
	}
	if r.Value.Kind != kind {
		return kindError(r.Attribute, kind, r.Value.describe())
	}
	// A set operand belongs to a set comparison and vice versa. Without this,
	// `source eq ["remux","bluray"]` would validate and then match nothing,
	// which is the worst outcome: a rule that looks right and silently never
	// fires.
	setOp := r.Op == OpIn || r.Op == OpNotIn
	switch {
	case setOp && !r.Value.isSetValue():
		return fmt.Errorf("%q compares against a list of names, like [\"remux\", \"bluray\"]", r.Op)
	case !setOp && r.Value.isSetValue():
		return fmt.Errorf("%q compares against a single name — use \"in\" for a list", r.Op)
	case setOp && len(r.Value.Texts) == 0:
		return fmt.Errorf("%q needs at least one name to compare against", r.Op)
	}

	// The three-section distinction, enforced.
	//
	// A weight on a gate is a category error and it is the mistake this design
	// most invites: someone reading §62's `"hevc": 20` reaches for a weight
	// everywhere. Silently ignoring it would mean an operator believes a gate
	// is scoring; refusing it says which section they wanted.
	switch section {
	case SectionPrefer:
		if r.Weight == 0 {
			return errors.New("a preference needs a non-zero weight — " +
				"a weight of 0 cannot change any score, so it is indistinguishable " +
				"from not writing the rule at all (a negative weight is a penalty, and is fine)")
		}
	default:
		if r.Weight != 0 {
			return fmt.Errorf("carries a weight of %d, but %s rules are not scored — "+
				"%s", r.Weight, section, sectionAdvice(section))
		}
	}
	return nil
}

func sectionAdvice(s Section) string {
	if s == SectionAccept {
		return "an accept rule is a GATE: it passes or the candidate is rejected. " +
			"Move it to `prefer` to score it"
	}
	return "a terminal rule is a STOP CONDITION: it holds or the search continues. " +
		"Move it to `prefer` to score it"
}
