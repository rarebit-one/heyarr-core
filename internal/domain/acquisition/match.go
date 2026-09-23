package acquisition

import (
	"fmt"
	"strconv"
	"strings"
)

// The season/episode match gate (ADR-0093 §2).
//
// # A match gate, deliberately NOT a quality gate
//
// This decides whether a release could CONTAIN the wanted episode at all,
// before §63 asks whether it is good enough. Season and episode are questions
// of identity and containment, not of quality, so they are NOT policy.Profile
// accept rules: folding them in would make "this is the wrong episode" and
// "this is the wrong resolution" the same kind of rejection, and an operator
// must be able to tell a season-blind match from an unmet quality bar apart.
// ADR-0093 rejects that alternative by name.
//
// # It still produces §63's reasons
//
// The reason this builds is the same Reason type §63 uses, with a stable
// machine-readable Rule code and human prose, so a match rejection sits
// alongside the quality reasons in the stored evaluation and in the API's
// rejected_by — "why did it grab an S03 pack for S01E01" has an answer that
// names the rule.
//
// # The parse happens in the caller, not here
//
// candidate.go's boundary is that a release title is never parsed in this
// package (§63's attributes come from the provider, ADR-0091). This gate keeps
// that boundary: the caller reads the release's season/episode with the
// scanner's parser (identification.ParseReleaseSeasonEpisode) and hands the
// NUMBERS here. This function reasons over numbers, never over a title.

// SectionMatch is the section a season/episode containment reason belongs to.
//
// Distinct from §62's accept/prefer/terminal so a match rejection is tellable
// apart from a quality one. RejectedBy includes it, because "rejected by" means
// "the gates that rejected this", and the match gate is one — the Section field
// is what still says which kind of gate it was.
const SectionMatch = "match"

// The match gate's stable rule codes. A client branches on these; the prose is
// for a human.
const (
	// RuleSeasonContains is the season half of the gate: a release for the
	// right season (an exact episode of it, or a pack for it) passes; a release
	// for a different season fails.
	RuleSeasonContains = "season.contains"
	// RuleEpisodeContains is the episode half: within the right season, the
	// exact episode (or a multi-episode file covering it) passes; another
	// episode fails.
	RuleEpisodeContains = "episode.contains"
	// RuleSeasonUndetermined fails a release whose season/episode could not be
	// read from its name at all — undetermined, so it cannot be shown to
	// contain the wanted episode.
	RuleSeasonUndetermined = "season.undetermined"
)

// GateEpisode decides whether a release can contain the wanted episode and
// returns the match Reason and whether the release is kept.
//
// determined/relSeason/relEpisodes are what the scanner's parser read off the
// release title (relEpisodes empty with determined true means a whole-season
// pack). The verdict:
//
//   - a release for a DIFFERENT season is rejected (RuleSeasonContains);
//   - a release for the SAME season that is either the exact episode or a
//     whole-season pack is kept;
//   - a release for the same season but a different episode is rejected
//     (RuleEpisodeContains);
//   - a release whose season/episode is UNDETERMINED is rejected
//     (RuleSeasonUndetermined) — the same safe direction Evaluate takes for an
//     undetermined accept gate: it never accepts what it cannot show to hold.
//
// It never keeps a release for a season other than the wanted one. The prose
// notes that the season/episode was read from the release name (ADR-0091's
// honesty property), the same "(read from the release name)" describe() adds.
func GateEpisode(wantSeason, wantEpisode int, determined bool, relSeason int, relEpisodes []int) (Reason, bool) {
	want := fmt.Sprintf("S%02dE%02d", wantSeason, wantEpisode)

	if !determined {
		return Reason{
			Rule: RuleSeasonUndetermined, Section: SectionMatch, Result: ResultUndetermined,
			Detail: "the release name does not spell out a season or episode, " +
				"so it cannot be shown to contain " + want,
		}, false
	}

	if relSeason != wantSeason {
		return Reason{
			Rule: RuleSeasonContains, Section: SectionMatch, Result: ResultFail,
			Detail: fmt.Sprintf("the release is season %d (read from the release name), "+
				"which cannot contain %s", relSeason, want),
		}, false
	}

	// Same season. A pack (no episode spelled out) contains every episode of it.
	if len(relEpisodes) == 0 {
		return Reason{
			Rule: RuleSeasonContains, Section: SectionMatch, Result: ResultPass,
			Detail: fmt.Sprintf("the release is a season %d pack (read from the release name), "+
				"which contains %s", relSeason, want),
		}, true
	}

	for _, e := range relEpisodes {
		if e == wantEpisode {
			return Reason{
				Rule: RuleEpisodeContains, Section: SectionMatch, Result: ResultPass,
				Detail: fmt.Sprintf("the release is %s (read from the release name), the wanted episode",
					want),
			}, true
		}
	}

	return Reason{
		Rule: RuleEpisodeContains, Section: SectionMatch, Result: ResultFail,
		Detail: fmt.Sprintf("the release is season %d episode %s (read from the release name), "+
			"which is not %s", relSeason, joinEpisodes(relEpisodes), want),
	}, false
}

// RejectedByMatch builds the Ranked entry for a candidate the match gate
// refused: an evaluation that is not accepted and carries exactly the gate's
// one reason. It is never scored — a release that cannot contain the episode is
// not a near miss on quality — so it competes with nothing and sorts last.
func RejectedByMatch(c ReleaseCandidate, reason Reason) Ranked {
	return Ranked{
		Candidate: c,
		Evaluation: Evaluation{
			CandidateID: c.ID,
			Accepted:    false,
			Reasons:     []Reason{reason},
		},
	}
}

// joinEpisodes renders a release's episode numbers for a rejection's prose.
func joinEpisodes(episodes []int) string {
	parts := make([]string, 0, len(episodes))
	for _, e := range episodes {
		parts = append(parts, strconv.Itoa(e))
	}
	return strings.Join(parts, ", ")
}
