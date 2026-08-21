package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The validation table.
//
// Every case here is a mistake someone will make, and the assertion is on the
// MESSAGE as well as the refusal — a validator that rejects everything with
// "invalid profile" passes a test that only checks for an error, and is
// useless to the person holding the profile.

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		// wantErr is a fragment the message must contain. Empty means the
		// profile must validate.
		wantErr string
	}{
		{
			name:    "a profile with no rules at all is legal",
			profile: Profile{Name: "empty"},
		},
		{
			name:    "a profile needs a name",
			profile: Profile{Accept: []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080)}}},
			wantErr: "needs a name",
		},
		{
			name:    "a name of only whitespace is not a name",
			profile: Profile{Name: "   "},
			wantErr: "needs a name",
		},

		// The acceptance criterion this issue names first: an accept rule
		// naming an unknown attribute is rejected at WRITE time, and the
		// message lists what it could have been.
		{
			name: "an unknown attribute is refused, and the message lists the real ones",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: "minimum_resolution", Op: OpGTE, Value: Num(1080)},
			}},
			wantErr: "no such attribute",
		},
		{
			name: "an unknown attribute in prefer is refused too",
			profile: Profile{Name: "p", Prefer: []Rule{
				{Attribute: "bitrate", Op: OpGTE, Value: Num(1), Weight: 5},
			}},
			wantErr: "no such attribute",
		},
		{
			name: "an unknown attribute in terminal is refused too",
			profile: Profile{Name: "p", Terminal: []Rule{
				{Attribute: "quality", Op: OpEq, Value: Text("best")},
			}},
			wantErr: "no such attribute",
		},

		// Kind and comparison.
		{
			name: "an ordering comparison on a text attribute is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrSource, Op: OpGTE, Value: Text("bluray")},
			}},
			wantErr: "does not apply",
		},
		{
			name: "a set comparison on a numeric attribute is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpIn, Value: Texts("1080")},
			}},
			wantErr: "does not apply",
		},
		{
			name: "a text operand on a numeric attribute is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Text("1080")},
			}},
			wantErr: "takes a int value",
		},
		{
			name: "a numeric operand on a flag attribute is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrHDR, Op: OpEq, Value: Num(1)},
			}},
			wantErr: "takes a flag value",
		},
		{
			name: "a rule with no operand at all is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE},
			}},
			wantErr: "needs a value",
		},
		{
			name: "a rule with no comparison is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Value: Num(1080)},
			}},
			wantErr: "needs a comparison",
		},

		// A rule that looks right and would silently never fire. This is the
		// worst class of mistake and the reason set-ness is checked both ways.
		{
			name: "a list operand with a single-name comparison is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrSource, Op: OpEq, Value: Texts("remux", "bluray")},
			}},
			wantErr: `use "in" for a list`,
		},
		{
			name: "a single name with a set comparison is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrSource, Op: OpIn, Value: Text("remux")},
			}},
			wantErr: "compares against a list",
		},
		{
			name: "an empty set is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrSource, Op: OpIn, Value: Texts()},
			}},
			wantErr: "at least one name",
		},

		// The three-section distinction. These are the cases that make the
		// gate/score/stop separation real rather than documentation.
		{
			name: "a weight on an accept rule is refused, and says where it belongs",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080), Weight: 20},
			}},
			wantErr: "accept rule is a GATE",
		},
		{
			name: "a weight on a terminal rule is refused, and says where it belongs",
			profile: Profile{Name: "p", Terminal: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160), Weight: 20},
			}},
			wantErr: "terminal rule is a STOP CONDITION",
		},
		{
			name: "a preference with no weight is refused",
			profile: Profile{Name: "p", Prefer: []Rule{
				{Attribute: AttrHDR, Op: OpEq, Value: Flag(true)},
			}},
			wantErr: "non-zero weight",
		},
		{
			name: "a negative weight is legal — a penalty is a real thing to want",
			profile: Profile{Name: "p", Prefer: []Rule{
				{Attribute: AttrSource, Op: OpEq, Value: Text("webrip"), Weight: -30},
			}},
		},

		// Duplicates.
		{
			name: "the same attribute and comparison twice in one section is refused",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080)},
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(720)},
			}},
			wantErr: "twice",
		},
		{
			name: "the same attribute with DIFFERENT comparisons is fine — that is a range",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(720)},
				{Attribute: AttrResolution, Op: OpLTE, Value: Num(2160)},
			}},
		},
		{
			name: "the same attribute in two different sections is fine",
			profile: Profile{
				Name:     "p",
				Accept:   []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080)}},
				Terminal: []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160)}},
			},
		},

		// A contradiction is deliberately NOT detected here. §63 reports it as
		// a rejection naming the rule that failed, which is legible; a
		// half-implemented contradiction check that misses most cases is not.
		{
			name: "a contradictory gate validates, and is left to be explained at evaluation",
			profile: Profile{Name: "p", Accept: []Rule{
				{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160)},
				{Attribute: AttrResolution, Op: OpLTE, Value: Num(720)},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.profile
			err := p.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected this profile to validate, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("the refusal should mention %q, but said: %v", tc.wantErr, err)
			}
		})
	}
}

// A refusal has to be findable. Every message names the section and the
// attribute, so that a profile with forty rules produces an error someone can
// act on rather than one they have to bisect.
func TestValidationErrorsNameTheSectionAndAttribute(t *testing.T) {
	p := Profile{Name: "p", Prefer: []Rule{
		{Attribute: AttrResolution, Op: OpGTE, Value: Num(1080), Weight: 5},
		{Attribute: AttrHDR, Op: OpEq, Value: Flag(true)},
	}}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected the weightless preference to be refused")
	}
	for _, want := range []string{"prefer", "rule 2", string(AttrHDR)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should locate the problem with %q, but said: %v", want, err)
		}
	}
}

// An absent `terminal` and an empty one are the SAME thing. This is the
// "never stop looking" case, and it must not need a sentinel value.
func TestTerminalAbsentAndEmptyAreTheSame(t *testing.T) {
	absent := Profile{Name: "a"}
	empty := Profile{Name: "b", Terminal: []Rule{}}
	for _, p := range []Profile{absent, empty} {
		if err := p.Validate(); err != nil {
			t.Fatalf("%s should validate: %v", p.Name, err)
		}
		if p.HasTerminal() {
			t.Errorf("%s has no terminal rules, so it must never be terminal", p.Name)
		}
	}

	withOne := Profile{Name: "c", Terminal: []Rule{
		{Attribute: AttrResolution, Op: OpGTE, Value: Num(2160)},
	}}
	if err := withOne.Validate(); err != nil {
		t.Fatal(err)
	}
	if !withOne.HasTerminal() {
		t.Error("a profile with a terminal rule can be finished")
	}
}

// Normalisation. Two operators spelling one capability differently must
// converge, or a profile works for whoever wrote it and silently rejects
// everything for the next person.
func TestNormalisation(t *testing.T) {
	p := Profile{
		Name: "  Living Room  ",
		Accept: []Rule{
			{Attribute: "  RESOLUTION ", Op: " GTE ", Value: Num(1080)},
		},
		Prefer: []Rule{
			{Attribute: AttrVideoCodec, Op: OpEq, Value: Text("  HEVC  "), Weight: 20},
			{Attribute: AttrSource, Op: OpIn, Value: Texts(" Remux ", "BluRay"), Weight: 5},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("normalisation should let this validate: %v", err)
	}
	if p.Name != "Living Room" {
		t.Errorf("the name should be trimmed, got %q", p.Name)
	}
	if p.Accept[0].Attribute != AttrResolution || p.Accept[0].Op != OpGTE {
		t.Errorf("the attribute and comparison should be folded, got %s %s",
			p.Accept[0].Attribute, p.Accept[0].Op)
	}
	if got := p.Prefer[0].Value.Text; got != "hevc" {
		t.Errorf("a text operand should be folded, got %q", got)
	}
	if got := p.Prefer[1].Value.Texts; got[0] != "remux" || got[1] != "bluray" {
		t.Errorf("every member of a set should be folded, got %v", got)
	}
}

func TestTooManyRules(t *testing.T) {
	p := Profile{Name: "p"}
	// Distinct rules, so the refusal is about the count rather than duplicates.
	for i := range maxRules + 1 {
		p.Prefer = append(p.Prefer, Rule{
			Attribute: AttrResolution, Op: OpGTE, Value: Num(int64(i)), Weight: 1,
		})
	}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "past the limit") {
		t.Fatalf("expected a limit refusal, got %v", err)
	}
}

// Rules() is the order everything downstream reports in. §63's determinism
// guarantee is not one if it stops at the evaluator and leaks at the
// reporting.
func TestRulesOrderIsStable(t *testing.T) {
	p := Defaults()[0]
	first := p.Rules()
	for range 50 {
		got := p.Rules()
		if len(got) != len(first) {
			t.Fatalf("Rules() returned %d then %d", len(first), len(got))
		}
		for i := range got {
			if got[i].Section != first[i].Section || got[i].Rule.String() != first[i].Rule.String() {
				t.Fatalf("Rules() is not stable at %d: %v then %v", i, first[i], got[i])
			}
		}
	}
	// Sections come out in §62's order, which is the order an operator reads.
	if first[0].Section != SectionAccept {
		t.Errorf("accept rules come first, got %s", first[0].Section)
	}
}

// The seeded set must actually be valid — a default profile that fails its own
// validator would make a fresh Heyarr fail to start, and would do it on
// someone else's machine rather than here.
func TestDefaultsValidate(t *testing.T) {
	defs := Defaults()
	if len(defs) == 0 {
		t.Fatal("a fresh Heyarr needs at least one profile to point a DesiredItem at")
	}
	names := map[string]bool{}
	for i := range defs {
		p := defs[i]
		if err := p.Validate(); err != nil {
			t.Errorf("the seeded profile %q does not validate: %v", p.Name, err)
		}
		if names[p.Name] {
			t.Errorf("two seeded profiles are called %q; seeding converges on the name", p.Name)
		}
		names[p.Name] = true
	}

	// The "never stop looking" case is exercised by a real default rather than
	// only by a unit test, so the path is live on every installation.
	var neverTerminal int
	for _, p := range defs {
		if !p.HasTerminal() {
			neverTerminal++
		}
	}
	if neverTerminal == 0 {
		t.Error("no seeded profile is open-ended, so nothing exercises the " +
			"never-terminal path outside a unit test")
	}
}

// The wire shape. A profile should read the way §62 writes one: bare values,
// no redundant type tag.
func TestValueJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Value
		want string
	}{
		{"a number", Num(1080), `1080`},
		{"a name", Text("remux"), `"remux"`},
		{"a set", Texts("remux", "bluray"), `["remux","bluray"]`},
		{"true", Flag(true), `true`},
		{"false", Flag(false), `false`},
		{"a negative number", Num(-5), `-5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("encoded as %s, want %s", raw, tc.want)
			}
			var back Value
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if back.String() != tc.in.String() || back.Kind != tc.in.Kind {
				t.Fatalf("round-tripped %s (%s) into %s (%s)",
					tc.in, tc.in.Kind, back, back.Kind)
			}
		})
	}
}

func TestValueRejectsFractionalNumbers(t *testing.T) {
	// Rounding a typo is how a typo becomes a working rule that means
	// something else.
	var v Value
	err := json.Unmarshal([]byte(`1080.5`), &v)
	if err == nil || !strings.Contains(err.Error(), "whole number") {
		t.Fatalf("expected a refusal naming whole numbers, got %v", err)
	}
}

func TestValueKindIsInferredFromTheJSONNotTheAttribute(t *testing.T) {
	// "1080" as a string must not be coerced into the number 1080. It has to
	// arrive as text and be refused by Validate against a numeric attribute,
	// which is what makes "1O80" a caught typo rather than a working rule.
	var v Value
	if err := json.Unmarshal([]byte(`"1080"`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindText {
		t.Fatalf(`"1080" should decode as text, got %s`, v.Kind)
	}
	p := Profile{Name: "p", Accept: []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: v}}}
	if err := p.Validate(); err == nil {
		t.Fatal("a quoted number against a numeric attribute must be refused")
	}
}

func TestProfileJSONRoundTrip(t *testing.T) {
	for _, want := range Defaults() {
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got Profile
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v", want.Name, err)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("%s does not survive a round trip: %v", want.Name, err)
		}
		if len(got.Accept) != len(want.Accept) ||
			len(got.Prefer) != len(want.Prefer) ||
			len(got.Terminal) != len(want.Terminal) {
			t.Fatalf("%s changed shape across a round trip", want.Name)
		}
		for i := range want.Prefer {
			if got.Prefer[i].Weight != want.Prefer[i].Weight {
				t.Errorf("%s: preference %d lost its weight (%d became %d)",
					want.Name, i, want.Prefer[i].Weight, got.Prefer[i].Weight)
			}
		}
	}
}

func TestKindOf(t *testing.T) {
	if _, ok := KindOf("nonsense"); ok {
		t.Error("an unknown attribute must not report a kind")
	}
	for _, a := range Attributes() {
		if _, ok := KindOf(a); !ok {
			t.Errorf("%s is listed but has no kind", a)
		}
	}
}
