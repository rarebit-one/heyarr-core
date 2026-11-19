package acquisition

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The behaviour #558's configurable language default must produce — and exactly
// what the hand-authored `everyday-en` profile did: a confirmed wanted-language
// release wins, an untagged release is still accepted (never rejected for an
// undetermined language, #129), and a confirmed foreign dub is penalised to
// last — all through prefer scoring, with no language ACCEPT gate anywhere.
func TestLanguageDefaultScoresPreferNotGate(t *testing.T) {
	base := policy.Profile{
		Name: "everyday",
		Accept: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(720)},
		},
		Terminal: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(1080)},
		},
	}
	p := base.WithLanguageDefault(policy.LanguageDefault{
		Prefer: "en", Weight: 50, Foreign: []string{"it", "es"}, Penalty: 100,
	})

	cand := func(id, lang string) ReleaseCandidate {
		attrs := Attributes{policy.AttrResolution: policy.Num(1080)}
		if lang != "" {
			attrs[policy.AttrLanguage] = policy.Text(lang)
		}
		return ReleaseCandidate{ID: id, Title: id, Provider: "test", Attributes: attrs}
	}

	en := Evaluate(cand("en", "en"), p)
	und := Evaluate(cand("und", ""), p)
	it := Evaluate(cand("it", "it"), p)

	// Nothing is rejected for its language — the whole point of prefer over a gate.
	for name, e := range map[string]Evaluation{"en": en, "undetermined": und, "it": it} {
		if !e.Accepted {
			t.Errorf("%s release was rejected; language must never gate: %+v", name, e)
		}
	}
	if en.Score != 50 {
		t.Errorf("confirmed-en should score +50, got %d", en.Score)
	}
	if und.Score != 0 {
		t.Errorf("an untagged release should be language-neutral, got %d", und.Score)
	}
	if it.Score != -100 {
		t.Errorf("a confirmed foreign dub should be penalised to -100, got %d", it.Score)
	}

	// And the total order the acquirer actually uses puts English first, dub last.
	ranked := EvaluateAll([]ReleaseCandidate{cand("it", "it"), cand("und", ""), cand("en", "en")}, p)
	if ranked[0].Candidate.ID != "en" || ranked[len(ranked)-1].Candidate.ID != "it" {
		t.Errorf("ranking should be en > undetermined > it, got %s ... %s",
			ranked[0].Candidate.ID, ranked[len(ranked)-1].Candidate.ID)
	}
}

// Language is a SET (#888): a release carrying several audio languages holds a
// language rule when any of them does, the way a multi-region dump does. So a
// Hindi-and-English release earns the English bonus AND the dub penalty, and
// with the penalty the larger it ranks below a plain English release and below
// an untagged one — while still never rejected for its language.
func TestALanguageSetEarnsTheBonusAndThePenaltyTogether(t *testing.T) {
	p := policy.Profile{
		Name: "everyday",
		Accept: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(720)},
		},
	}.WithLanguageDefault(policy.LanguageDefault{
		Prefer: "en", Weight: 50, Foreign: []string{"it", "hi"}, Penalty: 100,
	})

	cand := func(id string, langs ...string) ReleaseCandidate {
		attrs := Attributes{policy.AttrResolution: policy.Num(1080)}
		if len(langs) > 0 {
			attrs[policy.AttrLanguage] = policy.Texts(langs...).AsDerived()
		}
		return ReleaseCandidate{ID: id, Title: id, Provider: "test", Attributes: attrs}
	}

	cases := []struct {
		id    string
		langs []string
		score int
	}{
		{"en", []string{"en"}, 50},
		{"und", nil, 0},
		{"hi-en", []string{"hi", "en"}, -50},
		{"hi", []string{"hi"}, -100},
		{"en-ja", []string{"en", "ja"}, 50}, // a language on neither list is neutral
	}
	var all []ReleaseCandidate
	for _, tc := range cases {
		e := Evaluate(cand(tc.id, tc.langs...), p)
		if !e.Accepted {
			t.Errorf("%s was rejected; language must never gate: %+v", tc.id, e)
		}
		if e.Score != tc.score {
			t.Errorf("%s scored %d, want %d: %+v", tc.id, e.Score, tc.score, e.Reasons)
		}
		all = append(all, cand(tc.id, tc.langs...))
	}

	ranked := EvaluateAll(all, p)
	pos := map[string]int{}
	for i, r := range ranked {
		pos[r.Candidate.ID] = i
	}
	if pos["en"] >= pos["und"] || pos["und"] >= pos["hi-en"] || pos["hi-en"] >= pos["hi"] {
		t.Errorf("ranking should be en > und > hi-en > hi, got positions %v", pos)
	}

	// §63 names the set, and that it was read from the release name.
	e := Evaluate(cand("hi-en", "hi", "en"), p)
	var named bool
	for _, r := range e.Reasons {
		if strings.Contains(r.Detail, "language [hi, en] (read from the release name)") {
			named = true
		}
	}
	if !named {
		t.Errorf("no reason names the derived language set: %+v", e.Reasons)
	}
}
