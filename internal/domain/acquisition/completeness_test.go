package acquisition

import (
	"strings"
	"testing"
)

// EvaluateCompleteness is the fold desired.go's Scope doc said was impossible
// without a metadata provider: once a feed adapter has enumerated the items a
// source should have, "every episode exists" is a count over the per-item
// verdicts rather than a new evaluator.
func TestEvaluateCompleteness(t *testing.T) {
	cases := []struct {
		name        string
		items       []ItemVerdict
		want        Satisfaction
		wantMissing []string
		wantDetail  string
	}{
		{
			// Nobody has enumerated this source yet. Unknown, NOT vacuously
			// satisfied — reporting an un-polled source as fully archived is the
			// same lie EvaluatePlacement refuses for an empty required set.
			name:       "no items enumerated is unknown",
			items:      nil,
			want:       SatisfactionUnknown,
			wantDetail: "no items",
		},
		{
			name: "every item held is satisfied",
			items: []ItemVerdict{
				{ItemID: "s01e01", Satisfaction: SatisfactionSatisfied},
				{ItemID: "s01e02", Satisfaction: SatisfactionSatisfied},
			},
			want:       SatisfactionSatisfied,
			wantDetail: "2 of 2",
		},
		{
			name: "one missing item makes the source incomplete",
			items: []ItemVerdict{
				{ItemID: "s01e01", Satisfaction: SatisfactionSatisfied},
				{ItemID: "s01e02", Satisfaction: SatisfactionNot},
			},
			want:        SatisfactionNot,
			wantMissing: []string{"s01e02"},
			wantDetail:  "1 of 2",
		},
		{
			// An item nobody has looked at yet is missing for completeness: the
			// source is not known-complete while any item is unexamined.
			name: "an unexamined item counts as missing",
			items: []ItemVerdict{
				{ItemID: "s01e01", Satisfaction: SatisfactionSatisfied},
				{ItemID: "s01e02", Satisfaction: SatisfactionUnknown},
			},
			want:        SatisfactionNot,
			wantMissing: []string{"s01e02"},
		},
		{
			name: "nothing held is not, and every item is missing",
			items: []ItemVerdict{
				{ItemID: "s01e02", Satisfaction: SatisfactionNot},
				{ItemID: "s01e01", Satisfaction: SatisfactionNot},
			},
			want:        SatisfactionNot,
			wantMissing: []string{"s01e01", "s01e02"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateCompleteness(tc.items)
			if got.Satisfaction != tc.want {
				t.Fatalf("satisfaction = %s, want %s (%s)", got.Satisfaction, tc.want, got.Detail)
			}
			if strings.Join(got.Missing, ",") != strings.Join(tc.wantMissing, ",") {
				t.Errorf("missing = %v, want %v", got.Missing, tc.wantMissing)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
			if strings.TrimSpace(got.Detail) == "" {
				t.Error("every verdict carries an explanation")
			}
		})
	}
}

// The missing list is stable, because it drives per-item wants and an order
// that depends on map iteration is one nobody can diff — the same discipline
// EvaluatePlacement's missing list keeps.
func TestCompletenessMissingIsOrdered(t *testing.T) {
	items := []ItemVerdict{
		{ItemID: "zulu", Satisfaction: SatisfactionNot},
		{ItemID: "alpha", Satisfaction: SatisfactionNot},
		{ItemID: "mike", Satisfaction: SatisfactionSatisfied},
	}
	for range 50 {
		got := EvaluateCompleteness(items)
		if strings.Join(got.Missing, ",") != "alpha,zulu" {
			t.Fatalf("missing = %v, want [alpha zulu]", got.Missing)
		}
	}
}

// Completeness is content-only: it never returns converging or not_applicable,
// which are placement's answers. Whole-set completeness is met or not.
func TestCompletenessUsesOnlyContentValues(t *testing.T) {
	allowed := map[Satisfaction]bool{}
	for _, v := range ContentValues() {
		allowed[v] = true
	}
	verdicts := []CompletenessVerdict{
		EvaluateCompleteness(nil),
		EvaluateCompleteness([]ItemVerdict{{ItemID: "a", Satisfaction: SatisfactionSatisfied}}),
		EvaluateCompleteness([]ItemVerdict{{ItemID: "a", Satisfaction: SatisfactionNot}}),
	}
	for _, v := range verdicts {
		if !allowed[v.Satisfaction] {
			t.Errorf("completeness returned %s, which is not a content-axis value", v.Satisfaction)
		}
	}
}
