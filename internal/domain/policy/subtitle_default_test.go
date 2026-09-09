package policy

import "testing"

// The seeded `subtitle` profile (ADR-0085) judges on a subtitle's only attribute
// — its size — and is terminal there, so a fetched caption satisfies at once and
// the upgrade loop does not re-fetch it forever. (Its end-to-end behaviour is
// exercised in the catalog reconcile tests; this pins the seeded shape.)
func TestDefaultsSeedsSubtitleProfile(t *testing.T) {
	var sub *Profile
	for i := range Defaults() {
		if Defaults()[i].Name == "subtitle" {
			p := Defaults()[i]
			sub = &p
			break
		}
	}
	if sub == nil {
		t.Fatal("Defaults() has no subtitle profile")
	}
	if len(sub.Accept) == 0 {
		t.Error("the subtitle profile has no accept rule, so it admits nothing")
	}
	if !sub.HasTerminal() {
		t.Error("the subtitle profile is not terminal, so a held caption would be re-fetched forever")
	}
	// It must not gate on a video attribute a subtitle can never have.
	for _, r := range append(append([]Rule{}, sub.Accept...), sub.Terminal...) {
		if r.Attribute != AttrSizeBytes {
			t.Errorf("subtitle profile references %q; a subtitle only has size", r.Attribute)
		}
	}
}
