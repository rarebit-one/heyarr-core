package desired

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	const (
		work    = "work-1"
		edition = "edition-1"
		profile = "profile-1"
	)

	cases := []struct {
		name    string
		item    Item
		wantErr string
	}{
		{
			name: "a work-scoped want",
			item: Item{Scope: ScopeWork, WorkID: work, QualityProfileID: profile},
		},
		{
			name: "an edition-scoped want",
			item: Item{Scope: ScopeEdition, WorkID: work, EditionID: edition, QualityProfileID: profile},
		},
		{
			name: "an absent scope defaults to the whole work",
			item: Item{WorkID: work, QualityProfileID: profile},
		},
		{
			name:    "a want must name a work",
			item:    Item{QualityProfileID: profile},
			wantErr: "must name the work",
		},
		{
			// The one that makes §56 answerable at all.
			name:    "a want must name a quality profile",
			item:    Item{WorkID: work},
			wantErr: "must name a quality profile",
		},
		{
			name:    "an edition scope with no edition is refused",
			item:    Item{Scope: ScopeEdition, WorkID: work, QualityProfileID: profile},
			wantErr: "must name the edition",
		},
		{
			// An unused id is the kind of field something later reads without
			// checking the scope.
			name: "a work scope carrying an edition is refused",
			item: Item{
				Scope: ScopeWork, WorkID: work, EditionID: edition, QualityProfileID: profile,
			},
			wantErr: "must not name an edition",
		},
		{
			name:    "an unknown scope is refused",
			item:    Item{Scope: "episode", WorkID: work, QualityProfileID: profile},
			wantErr: "scope must be one of",
		},
		{
			name: "a reason is optional",
			item: Item{WorkID: work, QualityProfileID: profile, Reason: "for the flight"},
		},
		{
			name: "an oversized reason is refused",
			item: Item{
				WorkID: work, QualityProfileID: profile,
				Reason: strings.Repeat("x", maxReason+1),
			},
			wantErr: "past the limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := tc.item
			err := item.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected this want to validate, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("the refusal should mention %q, but said: %v", tc.wantErr, err)
			}
		})
	}
}

// The requirement most easily lost, because every fixture in the repository has
// assets: a want must be expressible for content with nothing behind it. A
// design that only works once something exists passes every test and fails the
// first real use.
func TestAWantNeedsNothingToExist(t *testing.T) {
	item := Item{WorkID: "a-work-with-no-assets", QualityProfileID: "profile-1"}
	if err := item.Validate(); err != nil {
		t.Fatalf("wanting content that does not exist is the whole point: %v", err)
	}
	kind, id := item.Target()
	if kind != "work" || id != "a-work-with-no-assets" {
		t.Errorf("Target() = (%s, %s), want (work, a-work-with-no-assets)", kind, id)
	}
}

// Target() is the pair every read path needs, and reading EditionID without
// checking Scope is the easy mistake it exists to prevent.
func TestTargetFollowsScope(t *testing.T) {
	work := Item{Scope: ScopeWork, WorkID: "w", QualityProfileID: "p"}
	if kind, id := work.Target(); kind != "work" || id != "w" {
		t.Errorf("work scope targets (%s, %s)", kind, id)
	}
	edition := Item{Scope: ScopeEdition, WorkID: "w", EditionID: "e", QualityProfileID: "p"}
	if kind, id := edition.Target(); kind != "edition" || id != "e" {
		t.Errorf("edition scope targets (%s, %s)", kind, id)
	}
}

// §61: never one version per title. The living-room copy and the phone copy of
// one film are two wants, and both must exist.
func TestSameWantIsPerTargetAndProfile(t *testing.T) {
	base := Item{Scope: ScopeWork, WorkID: "w", QualityProfileID: "living-room"}

	sameTargetDifferentProfile := base
	sameTargetDifferentProfile.QualityProfileID = "everyday"
	if SameWant(base, sameTargetDifferentProfile) {
		t.Error("two profiles over one work are two wants — this is the §61 rule")
	}

	duplicate := base
	duplicate.ID = "a different row"
	duplicate.Monitor = true
	duplicate.Reason = "written twice"
	if !SameWant(base, duplicate) {
		t.Error("the same target and profile is one want, whatever else differs")
	}

	otherWork := base
	otherWork.WorkID = "w2"
	if SameWant(base, otherWork) {
		t.Error("different works are different wants")
	}

	// A work-scoped want and an edition-scoped want are never the same want,
	// even when the ids happen to collide across the two tables.
	workScoped := Item{Scope: ScopeWork, WorkID: "x", QualityProfileID: "p"}
	editionScoped := Item{Scope: ScopeEdition, WorkID: "w", EditionID: "x", QualityProfileID: "p"}
	if SameWant(workScoped, editionScoped) {
		t.Error("scope is part of identity — an id is only unique within its table")
	}
}

// Monitored and wanted are two axes (§60 keeps both words). Validation must not
// quietly couple them.
func TestMonitorIsIndependentOfWanting(t *testing.T) {
	for _, monitor := range []bool{true, false} {
		item := Item{WorkID: "w", QualityProfileID: "p", Monitor: monitor}
		if err := item.Validate(); err != nil {
			t.Errorf("monitor=%v should be valid: %v", monitor, err)
		}
	}
}

func TestValidateTrimsItsInputs(t *testing.T) {
	item := Item{
		Scope: ScopeWork, WorkID: "  w  ", QualityProfileID: " p ", Reason: "  why  ",
	}
	if err := item.Validate(); err != nil {
		t.Fatal(err)
	}
	if item.WorkID != "w" || item.QualityProfileID != "p" || item.Reason != "why" {
		t.Errorf("inputs were not trimmed: %+v", item)
	}
}

// A whitespace-only work id is not a work id. Without trimming before the
// emptiness check, " " would pass and produce a foreign key failure at write
// time instead of a message naming the field.
func TestWhitespaceIsNotAnIdentifier(t *testing.T) {
	item := Item{WorkID: "   ", QualityProfileID: "p"}
	if err := item.Validate(); err == nil || !strings.Contains(err.Error(), "must name the work") {
		t.Fatalf("expected a refusal naming the work, got %v", err)
	}
}

func TestParseScope(t *testing.T) {
	for _, s := range Scopes() {
		if got, err := ParseScope(string(s)); err != nil || got != s {
			t.Errorf("ParseScope(%q) = (%v, %v)", s, got, err)
		}
	}
	if _, err := ParseScope("episode"); err == nil {
		t.Error("episode is not a scope Heyarr can express yet — see the Scope doc")
	}
}
