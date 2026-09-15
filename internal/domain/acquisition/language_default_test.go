package acquisition

import (
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
