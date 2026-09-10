package acquisition

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The season/episode containment gate (ADR-0093 §2). The crux is that it must
// NEVER keep a release for a season other than the wanted one, so every case
// names the season/episode a release parsed to and whether it is kept.
func TestGateEpisode(t *testing.T) {
	const wantSeason, wantEpisode = 1, 1

	cases := []struct {
		name        string
		determined  bool
		relSeason   int
		relEpisodes []int
		wantKept    bool
		wantRule    string
		wantResult  Result
	}{
		{
			name: "exact episode is kept", determined: true, relSeason: 1, relEpisodes: []int{1},
			wantKept: true, wantRule: RuleEpisodeContains, wantResult: ResultPass,
		},
		{
			name: "season pack for the wanted season is kept", determined: true, relSeason: 1,
			wantKept: true, wantRule: RuleSeasonContains, wantResult: ResultPass,
		},
		{
			name: "multi-episode file covering it is kept", determined: true, relSeason: 1, relEpisodes: []int{1, 2},
			wantKept: true, wantRule: RuleEpisodeContains, wantResult: ResultPass,
		},
		{
			name: "a different season is rejected", determined: true, relSeason: 3, relEpisodes: []int{1},
			wantKept: false, wantRule: RuleSeasonContains, wantResult: ResultFail,
		},
		{
			name: "a wrong-season pack is rejected", determined: true, relSeason: 3,
			wantKept: false, wantRule: RuleSeasonContains, wantResult: ResultFail,
		},
		{
			name: "the same season, wrong episode is rejected", determined: true, relSeason: 1, relEpisodes: []int{5},
			wantKept: false, wantRule: RuleEpisodeContains, wantResult: ResultFail,
		},
		{
			name: "an undetermined release is rejected", determined: false,
			wantKept: false, wantRule: RuleSeasonUndetermined, wantResult: ResultUndetermined,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, kept := GateEpisode(wantSeason, wantEpisode, tc.determined, tc.relSeason, tc.relEpisodes)
			if kept != tc.wantKept {
				t.Fatalf("GateEpisode kept = %v, want %v (reason: %s)", kept, tc.wantKept, reason.Detail)
			}
			if reason.Rule != tc.wantRule {
				t.Errorf("rule = %q, want %q", reason.Rule, tc.wantRule)
			}
			if reason.Result != tc.wantResult {
				t.Errorf("result = %q, want %q", reason.Result, tc.wantResult)
			}
			if reason.Section != SectionMatch {
				t.Errorf("section = %q, want %q", reason.Section, SectionMatch)
			}
			if reason.Detail == "" {
				t.Error("a reason must carry prose a human can read")
			}
		})
	}
}

// A never-accept property: for every parsed season other than the wanted one,
// and for a pack or an exact episode of it, the gate must refuse. This guards
// the one invariant that matters most — an episode want must not pick up a
// different season's bytes.
func TestGateEpisodeNeverKeepsAnotherSeason(t *testing.T) {
	for relSeason := 0; relSeason <= 12; relSeason++ {
		if relSeason == 2 {
			continue // the wanted season, tested above
		}
		for _, eps := range [][]int{nil, {1}, {3}, {1, 2}} {
			if _, kept := GateEpisode(2, 1, true, relSeason, eps); kept {
				t.Errorf("kept a season %d release for an S02E01 want (episodes %v)", relSeason, eps)
			}
		}
	}
}

// A match rejection surfaces in RejectedBy alongside the quality rejections, so
// "why was this refused" has one answer — with the match section telling it
// apart from a quality gate.
func TestMatchRejectionIsReportedAsRejectedBy(t *testing.T) {
	reason, kept := GateEpisode(1, 1, true, 3, nil)
	if kept {
		t.Fatal("an S03 pack must not be kept for an S01E01 want")
	}
	ranked := RejectedByMatch(ReleaseCandidate{ID: "s03-pack"}, reason)
	if ranked.Evaluation.Accepted {
		t.Error("a match-rejected candidate must not be accepted")
	}
	rejected := ranked.Evaluation.RejectedBy()
	if len(rejected) != 1 {
		t.Fatalf("RejectedBy returned %d reasons, want 1", len(rejected))
	}
	if rejected[0].Section != SectionMatch || rejected[0].Rule != RuleSeasonContains {
		t.Errorf("rejected_by = %+v, want a match/season.contains reason", rejected[0])
	}
	if !strings.Contains(rejected[0].Detail, "read from the release name") {
		t.Errorf("the reason must say the season was read from the name: %q", rejected[0].Detail)
	}
	// And it must not masquerade as a quality accept rejection.
	for _, r := range rejected {
		if r.Section == string(policy.SectionAccept) {
			t.Error("a season mismatch must not be reported as a quality gate")
		}
	}
}
