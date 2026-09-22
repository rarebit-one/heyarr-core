package policy

import "testing"

// A disabled default (no language named) changes nothing — the status quo for
// an operator who does not care about language.
func TestLanguageDefaultDisabledProducesNoRules(t *testing.T) {
	if rules := (LanguageDefault{}).PreferRules(); len(rules) != 0 {
		t.Errorf("a zero LanguageDefault produced rules: %v", rules)
	}
	if (LanguageDefault{Foreign: []string{"it"}, Penalty: 100}).Enabled() {
		t.Error("a default with a penalty list but no Prefer is not a preference")
	}
}

// An enabled default renders exactly two prefer rules: a bonus for the wanted
// language and a penalty (negative weight) for the unwanted ones — never an
// accept gate (#129: an undetermined language gate fails closed).
func TestLanguageDefaultRendersPreferRules(t *testing.T) {
	d := LanguageDefault{Prefer: "en", Weight: 50, Foreign: []string{"it", "es"}, Penalty: 100}
	rules := d.PreferRules()
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d: %v", len(rules), rules)
	}
	for _, r := range rules {
		if r.Attribute != AttrLanguage {
			t.Errorf("rule is not about language: %v", r)
		}
	}
	if rules[0].Op != OpEq || rules[0].Weight != 50 {
		t.Errorf("bonus rule wrong: %v", rules[0])
	}
	if rules[1].Op != OpIn || rules[1].Weight != -100 {
		t.Errorf("penalty rule must apply the penalty as a NEGATIVE weight: %v", rules[1])
	}
}

// A profile that already states its own language rule is the author's explicit
// choice and overrides the server default (§558 layer 2 over layer 1).
func TestWithLanguageDefaultRespectsExplicitOverride(t *testing.T) {
	base := Profile{
		Name:   "wants-italian",
		Accept: []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: Num(720)}},
		Prefer: []Rule{{Attribute: AttrLanguage, Op: OpEq, Value: Text("it"), Weight: 50}},
	}
	got := base.WithLanguageDefault(LanguageDefault{Prefer: "en", Weight: 50, Foreign: []string{"it"}, Penalty: 100})
	if len(got.Prefer) != 1 || got.Prefer[0].Value.Text != Text("it").Text {
		t.Errorf("an explicit language choice must not be overridden by the default: %v", got.Prefer)
	}
}

// A video profile with no language opinion gets the default's rules appended,
// its existing rules untouched.
func TestWithLanguageDefaultAppendsToVideoProfile(t *testing.T) {
	base := Profile{
		Name:   "everyday",
		Accept: []Rule{{Attribute: AttrResolution, Op: OpGTE, Value: Num(720)}},
		Prefer: []Rule{{Attribute: AttrVideoCodec, Op: OpEq, Value: Text("hevc"), Weight: 15}},
	}
	got := base.WithLanguageDefault(LanguageDefault{Prefer: "en", Weight: 50, Foreign: []string{"it"}, Penalty: 100})
	if len(got.Prefer) != 3 {
		t.Fatalf("want 3 prefer rules (hevc + en bonus + foreign penalty), got %d: %v", len(got.Prefer), got.Prefer)
	}
	if got.Prefer[0].Attribute != AttrVideoCodec {
		t.Error("the existing prefer rule should be preserved and come first")
	}
	// Purity: the receiver's slice must not have grown.
	if len(base.Prefer) != 1 {
		t.Error("WithLanguageDefault mutated its receiver")
	}
}

// WithLanguageDefaults touches video profiles and leaves the others — a
// subtitle or an article has no audio language to prefer.
func TestWithLanguageDefaultsSkipsNonVideoProfiles(t *testing.T) {
	d := LanguageDefault{Prefer: "en", Weight: 50, Foreign: []string{"it"}, Penalty: 100}
	got := WithLanguageDefaults(Defaults(), d)
	for _, p := range got {
		switch p.Name {
		case "published", "subtitle":
			if p.mentionsLanguage() {
				t.Errorf("%s must not gain a language rule", p.Name)
			}
		case "everyday", "living-room", "archival":
			if !p.mentionsLanguage() {
				t.Errorf("%s should have gained the language default", p.Name)
			}
		}
	}
}
