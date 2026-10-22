package acquisition

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// A set-valued attribute holds a rule when ANY member does (ADR-0121 §5): a
// "(USA, Europe)" dump is a USA dump for an eq/in rule, and not one for neq/nin.
func TestASetValuedAttributeMatchesAnyMember(t *testing.T) {
	regions := policy.Texts("usa", "europe")
	for _, tc := range []struct {
		op    policy.Op
		value policy.Value
		want  bool
	}{
		{policy.OpEq, policy.Text("usa"), true},
		{policy.OpEq, policy.Text("japan"), false},
		{policy.OpNeq, policy.Text("usa"), false},
		{policy.OpIn, policy.Texts("japan", "europe"), true},
		{policy.OpIn, policy.Texts("japan"), false},
		{policy.OpNotIn, policy.Texts("japan"), true},
		{policy.OpNotIn, policy.Texts("europe"), false},
	} {
		if got := holds(regions, policy.Rule{Attribute: policy.AttrRegion, Op: tc.op, Value: tc.value}); got != tc.want {
			t.Errorf("[usa, europe] %s %s = %v, want %v", tc.op, tc.value, got, tc.want)
		}
	}
}

// `rom-indexed` ranks by region, USA then World then Europe, never gates on it,
// and is terminal once anything is held, so a held dump is never chased by
// another region's.
func TestROMIndexedPrefersUSAThenWorldThenEurope(t *testing.T) {
	var profile policy.Profile
	for _, p := range policy.Defaults() {
		if p.Name == "rom-indexed" {
			profile = p
		}
	}
	score := func(regions ...string) Evaluation {
		attrs := Attributes{policy.AttrSizeBytes: policy.Num(1 << 20)}
		if len(regions) > 0 {
			attrs[policy.AttrRegion] = policy.Texts(regions...).AsDerived()
		}
		return Evaluate(ReleaseCandidate{ID: "r", Attributes: attrs}, profile)
	}
	usa, world, europe, japan, none := score("usa"), score("world"), score("europe"), score("japan"), score()
	if usa.Score <= world.Score || world.Score <= europe.Score || europe.Score <= japan.Score {
		t.Errorf("scores usa=%d world=%d europe=%d japan=%d, want strictly descending", usa.Score, world.Score, europe.Score, japan.Score)
	}
	if japan.Score != none.Score {
		t.Errorf("a Japan dump scored %d, an untagged one %d: neither is preferred, so they must tie", japan.Score, none.Score)
	}
	for name, e := range map[string]Evaluation{"japan": japan, "untagged": none} {
		if !e.Accepted || !e.Terminal {
			t.Errorf("%s: accepted=%v terminal=%v — region must rank, never gate or keep a want open", name, e.Accepted, e.Terminal)
		}
	}
	if both := score("usa", "europe"); both.Score < usa.Score {
		t.Errorf("a (USA, Europe) dump scored %d below a USA one (%d)", both.Score, usa.Score)
	}
	if mixed := score("europe", "japan"); mixed.Score != europe.Score {
		t.Errorf("a (Europe, Japan) dump scored %d, a Europe one %d: an unpreferred region must not cost a preferred one", mixed.Score, europe.Score)
	}
}
