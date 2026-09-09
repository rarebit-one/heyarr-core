package desired

import "testing"

// The aspect (ADR-0085): a subtitle is a distinct want on the same target,
// named by its aspect and language, and validated so a subtitle always carries a
// language and a concrete scope while a primary want carries neither.

func TestAspectDefaultsToPrimary(t *testing.T) {
	i := Item{Scope: ScopeWork, WorkID: "w1", QualityProfileID: "q1"}
	if err := i.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if i.Aspect != AspectPrimary {
		t.Errorf("aspect = %q, want primary", i.Aspect)
	}
}

func TestSubtitleWantRequiresLanguage(t *testing.T) {
	i := Item{Scope: ScopeItem, WorkID: "w1", ItemID: "it1", Aspect: AspectSubtitle, QualityProfileID: "q1"}
	if err := i.Validate(); err == nil {
		t.Fatal("a subtitle want with no language validated")
	}
}

func TestSubtitleWantLowercasesLanguage(t *testing.T) {
	i := Item{
		Scope: ScopeItem, WorkID: "w1", ItemID: "it1", Aspect: AspectSubtitle,
		Language: "EN", QualityProfileID: "q1",
	}
	if err := i.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if i.Language != "en" {
		t.Errorf("language = %q, want en", i.Language)
	}
}

func TestSubtitleWantRefusedAtWorkScope(t *testing.T) {
	// A work is a series/film, not a concrete video to caption (ADR-0085).
	i := Item{Scope: ScopeWork, WorkID: "w1", Aspect: AspectSubtitle, Language: "en", QualityProfileID: "q1"}
	if err := i.Validate(); err == nil {
		t.Fatal("a work-scoped subtitle want validated")
	}
}

func TestSubtitleWantAllowedAtItemAndEditionScope(t *testing.T) {
	for _, i := range []Item{
		{Scope: ScopeItem, WorkID: "w1", ItemID: "it1", Aspect: AspectSubtitle, Language: "en", QualityProfileID: "q1"},
		{Scope: ScopeEdition, WorkID: "w1", EditionID: "e1", Aspect: AspectSubtitle, Language: "en", QualityProfileID: "q1"},
	} {
		if err := i.Validate(); err != nil {
			t.Errorf("scope %q subtitle want: %v", i.Scope, err)
		}
	}
}

func TestPrimaryWantRefusesLanguage(t *testing.T) {
	i := Item{Scope: ScopeItem, WorkID: "w1", ItemID: "it1", Language: "en", QualityProfileID: "q1"}
	if err := i.Validate(); err == nil {
		t.Fatal("a primary want carrying a language validated")
	}
}

func TestSameWantFoldsAspectAndLanguage(t *testing.T) {
	base := Item{Scope: ScopeItem, WorkID: "w1", ItemID: "it1", QualityProfileID: "q1"}
	primary := base
	subEN := base
	subEN.Aspect, subEN.Language = AspectSubtitle, "en"
	subDE := base
	subDE.Aspect, subDE.Language = AspectSubtitle, "de"

	if SameWant(primary, subEN) {
		t.Error("a primary want and a subtitle want on one target are the same want")
	}
	if SameWant(subEN, subDE) {
		t.Error("the English and German subtitle of one target are the same want")
	}
	if !SameWant(subEN, subEN) {
		t.Error("a subtitle want is not the same as itself")
	}
	// The empty aspect normalises to primary, so a pre-aspect row and an explicit
	// primary want are one want.
	explicitPrimary := primary
	explicitPrimary.Aspect = AspectPrimary
	if !SameWant(primary, explicitPrimary) {
		t.Error("an empty aspect and an explicit primary aspect are different wants")
	}
}
